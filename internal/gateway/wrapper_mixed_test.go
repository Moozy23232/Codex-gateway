//go:build linux || darwin

package gateway

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func newMixedWrapperFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	f := newLifecycleFixture(t)
	f.cfg.Providers["official"] = Provider{Auth: "codex", BaseURL: officialBaseURL, AutoModels: false}
	f.cfg.Models["official/model"] = Model{Provider: "official", Model: "official-upstream", Template: "test-template"}
	f.cfg.DefaultModel = "official/model"
	// AutoModels=false alone is insufficient: a valid login enables it again.
	// The lifecycle fixture's fake auth lacks account_id, so no sync/RPC occurs.
	if _, err := readOfficialCredential(f.cfg); err == nil {
		t.Fatal("mixed wrapper fixture must not contain a usable official login")
	}
	if err := SaveConfig(f.home, f.cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_GATEWAY_LIFECYCLE_HELPER", "1")
	t.Setenv("CODEX_GATEWAY_OFFICIAL_HELPER", "")
	t.Setenv("CODEX_GATEWAY_WRAPPER_INVOCATION", "")
	t.Setenv("CODEX_GATEWAY_TOKEN", "")
	t.Setenv("CODEX_GATEWAY_TEST_EXIT", "37")
	t.Setenv("CODEX_GATEWAY_HOME", f.home)
	t.Setenv("CODEX_HOME", f.cfg.CodexHome)
	t.Setenv("CODEX_GATEWAY_CODEX_BIN", lifecycleFakeExecutable(t))
	t.Setenv("CODEX_GATEWAY_NATIVE_CODEX_BIN", os.Getenv("CODEX_GATEWAY_CODEX_BIN"))
	t.Setenv("CODEX_GATEWAY_TEST_RECORD", filepath.Join(f.home, "launch.json"))
	f.cleanupServer(t)
	return f
}

func checkMixedWrapperLaunch(t *testing.T, f *lifecycleFixture, args []string, wantModel string, gateway bool) []string {
	t.Helper()
	before := map[string][]byte{}
	for _, name := range []string{"config.toml", "auth.json"} {
		data, err := os.ReadFile(filepath.Join(f.cfg.CodexHome, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
	}
	var out, errOut bytes.Buffer
	if code := executeWrapped(args, strings.NewReader(""), &out, &errOut); code != 37 {
		t.Fatalf("wrapper exit = %d, stdout=%s, stderr=%s", code, &out, &errOut)
	}
	var observed struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env"`
	}
	if err := readJSON(filepath.Join(f.home, "launch.json"), &observed, true); err != nil {
		t.Fatal(err)
	}
	if usesHistoryPicker(args) {
		wantModel, gateway = "", false
	} // Selection only; routes and models are resolved after choosing.
	if got := selectedWrappedModel(observed.Args); got != wantModel {
		t.Errorf("selected model = %q, want %q; args=%q", got, wantModel, observed.Args)
	}
	if observed.Env["CODEX_HOME"] != f.cfg.CodexHome {
		t.Errorf("CODEX_HOME = %q, want %q", observed.Env["CODEX_HOME"], f.cfg.CodexHome)
	}
	overrides, _, err := lifecycleConfigArguments(observed.Args)
	if err != nil {
		t.Fatal(err)
	}
	if gateway {
		if observed.Env["CODEX_GATEWAY_TOKEN"] != lifecycleTestClient {
			t.Error("gateway launch did not receive its local client token")
		}
		for _, setting := range []string{
			`model_provider="codex-gateway"`,
			"model_providers.codex-gateway.base_url=" + lifecycleJSONString(lifecycleURL(f.cfg.Listen)+"/v1"),
			"model_catalog_json=" + lifecycleJSONString(filepath.Join(f.home, "models.json")),
		} {
			if !usesHistoryPicker(args) && !slices.Contains(overrides, setting) {
				t.Errorf("gateway launch missing %s; args=%q", setting, observed.Args)
			}
		}
		catalog, err := LoadCatalog(f.home)
		if err != nil {
			t.Fatal(err)
		}
		slugs := map[string]bool{}
		for _, model := range catalog.Models {
			slug, _ := model["slug"].(string)
			slugs[slug] = true
		}
		if !slugs["official/model"] || !slugs["example/model"] {
			t.Errorf("mixed /model catalog lost a provider: %v", slugs)
		}
		if state, err := Status(f.home); err != nil || state["running"] != true {
			t.Errorf("gateway not running: %v, %v", state, err)
		}
	} else {
		if observed.Env["CODEX_GATEWAY_TOKEN"] != "" {
			t.Error("native launch received a gateway token")
		}
		for _, override := range overrides {
			if strings.HasPrefix(override, "model_provider=") || strings.HasPrefix(override, "model_catalog_json=") {
				t.Errorf("native launch received gateway settings: %q", observed.Args)
			}
		}
		if _, err := os.Stat(filepath.Join(f.home, "runtime")); !os.IsNotExist(err) {
			t.Errorf("native launch created gateway runtime: %v", err)
		}
	}
	for name, data := range before {
		after, err := os.ReadFile(filepath.Join(f.cfg.CodexHome, name))
		if err != nil || !bytes.Equal(data, after) {
			t.Errorf("wrapper changed native %s: %v", name, err)
		}
	}
	command, _ := wrappedCommand(args)
	if usesHistoryPicker(args) {
		cwd, _ := os.Getwd()
		if len(observed.Args) < 4 || observed.Args[0] != "-C" || observed.Args[1] != cwd || observed.Args[2] != "--remote" || !strings.HasPrefix(observed.Args[3], "unix://") {
			t.Fatalf("missing local native history bridge: %q", observed.Args)
		}
		if _, err := os.Stat(strings.TrimPrefix(observed.Args[3], "unix://")); !os.IsNotExist(err) {
			t.Errorf("history bridge socket was not cleaned up: %v", err)
		}
		want := []string{command}
		if nativeHasFlag(args, "--all") {
			want = append(want, "--all")
		}
		if command == "resume" {
			want = append(want, "--include-non-interactive")
		}
		if !reflect.DeepEqual(observed.Args[4:], want) {
			t.Fatalf("picker arguments = %q, want %q", observed.Args[4:], want)
		}
	}
	return observed.Args
}

func TestWrapperMixedModelLaunches(t *testing.T) {
	for _, test := range []struct {
		name         string
		args         []string
		model        string
		gateway      bool
		officialOnly bool
		unconfigured bool
		wantNative   []string
	}{
		{name: "official alias", args: []string{"-m", "official/model", "exec", "prompt"}, model: "official/model", gateway: true},
		{name: "official upstream", args: []string{"-m", "official-upstream", "exec", "prompt"}, model: "official/model", gateway: true},
		{name: "official config alias", args: []string{"-c", `model="official/model"`, "exec", "prompt"}, model: "official/model", gateway: true},
		{name: "official config upstream", args: []string{"-c", `model="official-upstream"`, "exec", "prompt"}, model: "official/model", gateway: true},
		{name: "default official", model: "official/model", gateway: true},
		{name: "third party alias", args: []string{"-m", "example/model", "exec", "prompt"}, model: "example/model", gateway: true},
		{name: "third party upstream", args: []string{"-m", "upstream-model", "exec", "prompt"}, model: "example/model", gateway: true},
		{name: "unknown explicit", args: []string{"-m", "external-model", "exec", "prompt"}, model: "external-model"},
		{name: "unknown config", args: []string{"-c", `model="external-model"`, "exec", "prompt"}, model: "external-model"},
		{name: "version utility", args: []string{"--version"}},
		{name: "login status utility", args: []string{"login", "status"}},
		{name: "help with official alias", args: []string{"-m", "official/model", "exec", "--help"}, model: "official/model"},
		{name: "official only alias", args: []string{"-m", "official/model", "exec", "prompt"}, model: "official-upstream", officialOnly: true, wantNative: []string{"-m", "official-upstream", "exec", "prompt"}},
		{name: "official only default", model: "official-upstream", officialOnly: true, wantNative: []string{"-m", "official-upstream"}},
		{name: "official only resume", args: []string{"resume", "--all"}, officialOnly: true},
		{name: "unconfigured resume", args: []string{"resume"}, unconfigured: true},
		{name: "unconfigured resume all", args: []string{"resume", "--all"}, unconfigured: true},
		{name: "unconfigured fork", args: []string{"fork"}, unconfigured: true},
		{name: "unconfigured explicit model picker", args: []string{"resume", "-m", "external-model"}, unconfigured: true},
		{name: "configured unknown model picker", args: []string{"resume", "-m", "external-model"}},
		{name: "official only fork", args: []string{"fork"}, officialOnly: true},
		{name: "native resume picker", args: []string{"resume"}, model: "official/model", gateway: true},
		{name: "native resume all", args: []string{"resume", "--all"}, model: "official/model", gateway: true},
		{name: "native resume last", args: []string{"resume", "--last"}, model: "official/model", gateway: true},
		{name: "native fork picker", args: []string{"fork"}, model: "official/model", gateway: true},
		{name: "native fork last", args: []string{"fork", "--last"}, model: "official/model", gateway: true},
		{name: "native exec last", args: []string{"exec", "resume", "--last", "--json", "-"}, model: "official/model", gateway: true},
		{name: "resume explicit model", args: []string{"resume", "-m", "example/model"}, model: "example/model", gateway: true},
		{name: "resume config model", args: []string{"resume", "-c", `model="example/model"`}, model: "example/model", gateway: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMixedWrapperFixture(t)
			if test.officialOnly {
				delete(f.cfg.Models, "example/model")
				delete(f.cfg.Providers, "example")
				if err := SaveConfig(f.home, f.cfg); err != nil {
					t.Fatal(err)
				}
			}
			if test.unconfigured {
				t.Cleanup(func() {
					if err := SaveConfig(f.home, f.cfg); err != nil {
						t.Error(err)
					}
				})
				if err := os.Remove(filepath.Join(f.home, "config.json")); err != nil {
					t.Fatal(err)
				}
			}
			got := checkMixedWrapperLaunch(t, f, test.args, test.model, test.gateway)
			if test.gateway && wrappedResumeIndex(test.args) >= 0 && !usesHistoryPicker(test.args) {
				_, remaining, _ := lifecycleConfigArguments(got)
				_, want, _ := lifecycleConfigArguments(test.args)
				if !reflect.DeepEqual(remaining, want) {
					t.Errorf("native resume arguments changed: %q, want %q", remaining, want)
				}
			}
			if !test.gateway && !usesHistoryPicker(test.args) {
				want := test.wantNative
				if want == nil {
					want = test.args
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("native args = %q, want %q", got, want)
				}
			}
		})
	}
}

func TestWrapperMixedResumeLaunches(t *testing.T) {
	for _, test := range []struct {
		name, historyModel, provider, explicit, wantModel string
		gateway                                           bool
	}{
		{"official alias history", "official/model", "openai", "", "official/model", true},
		{"official upstream history", "official-upstream", "openai", "", "official/model", true},
		{"third party upstream history", "upstream-model", "example", "", "example/model", true},
		{"unknown gateway history", "removed-model", "codex-gateway", "", "", false},
		{"empty gateway history", "", "codex-gateway", "", "", false},
		{"official gateway history", "official/model", "codex-gateway", "", "official/model", true},
		{"third party native history", "example/model", "openai", "", "example/model", true},
		{"third party gateway history", "example/model", "codex-gateway", "", "example/model", true},
		{"external native history", "external-model", "external", "", "", false},
		{"official overrides third party history", "example/model", "codex-gateway", "official/model", "official/model", true},
		{"third party overrides official history", "official/model", "openai", "example/model", "example/model", true},
		{"official upstream overrides external history", "external-model", "external", "official-upstream", "official/model", true},
		{"unknown overrides official history", "official/model", "codex-gateway", "external-model", "external-model", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMixedWrapperFixture(t)
			session := wrappedSession{ID: "saved-id", Model: test.historyModel, ModelProvider: test.provider}
			reply, err := json.Marshal(map[string]any{"id": 2, "result": map[string]any{"thread": session}})
			if err != nil {
				t.Fatal(err)
			}
			// Only implement the fixed initialize/initialized/thread-read exchange;
			// do not extend TestMain or invoke an installed native Codex binary.
			rpc := filepath.Join(f.home, "history-rpc")
			script := "#!/bin/sh\n" +
				"IFS= read -r request || exit 1\n" +
				"printf '%s\\n' '{\"id\":1,\"result\":{}}'\n" +
				"IFS= read -r notification || exit 1\n" +
				"IFS= read -r request || exit 1\n" +
				"case \"$request\" in\n" +
				"  *'\"method\":\"thread/read\"'*) printf '%s\\n' '" + strings.ReplaceAll(string(reply), "'", "'\\''") + "' ;;\n" +
				"  *) exit 2 ;;\nesac\n"
			if err := os.WriteFile(rpc, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_GATEWAY_NATIVE_CODEX_BIN", rpc)
			args := []string{"resume", session.ID}
			if test.explicit != "" {
				args = append([]string{"-m", test.explicit}, args...)
			}
			got := checkMixedWrapperLaunch(t, f, args, test.wantModel, test.gateway)
			if test.gateway {
				if len(got) < 2 || !reflect.DeepEqual(got[len(got)-2:], []string{"resume", session.ID}) {
					t.Errorf("resume ID/command changed: %q", got)
				}
			} else if !reflect.DeepEqual(got, args) {
				t.Errorf("native resume args = %q, want %q", got, args)
			}
		})
	}
}

func TestWrapperResumeDoesNotResolveAmbiguityUsingCurrentDefault(t *testing.T) {
	cfg := &Config{
		DefaultModel: "example/shared",
		Providers:    map[string]Provider{"official": {Auth: "codex"}, "example": {Auth: "api_key"}},
		Models: map[string]Model{
			"official/shared": {Provider: "official", Model: "shared"},
			"example/shared":  {Provider: "example", Model: "shared"},
		},
	}
	for provider, want := range map[string]string{"openai": "official/shared", "example": "example/shared", "codex-gateway": "", "unknown": ""} {
		if got := wrappedSessionModel(wrappedSession{Model: "shared", ModelProvider: provider}, cfg); got != want {
			t.Errorf("history provider %q resolved to %q, want %q", provider, got, want)
		}
	}
	cfg.Models["other-official-alias"] = cfg.Models["official/shared"]
	if got := wrappedSessionModel(wrappedSession{Model: "shared", ModelProvider: "openai"}, cfg); got != "" {
		t.Errorf("ambiguous official history silently selected %q", got)
	}
}

func TestWrapperMixedSessionModel(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"official": {Auth: "codex"}, "example": {Auth: "api_key"}},
		Models:    map[string]Model{"official/model": {Provider: "official"}, "example/model": {Provider: "example"}},
	}
	for _, model := range []string{"official/model", "example/model", "unregistered", ""} {
		for _, provider := range []string{"openai", "official", "example", "codex-gateway", "external", ""} {
			t.Run(model+"/"+provider, func(t *testing.T) {
				want := ""
				if _, registered := cfg.Models[model]; registered {
					want = model
				}
				if got := wrappedSessionModel(wrappedSession{Model: model, ModelProvider: provider}, cfg); got != want {
					t.Errorf("wrappedSessionModel(model=%q, provider=%q) = %q, want %q", model, provider, got, want)
				}
			})
		}
	}
}
