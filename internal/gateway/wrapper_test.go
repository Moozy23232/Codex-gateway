package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestWrappedNativeCommandsAndArgumentParsing(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"-V"}, {"login"}, {"logout"}, {"-c", "model=x", "login", "status"}, {"exec", "--help"}, {"resume", "--help"}, {"--remote", "unix:///tmp/server", "resume"}, {"mcp", "list"}, {"help", "exec"}, {"--oss"}} {
		if !nativePassthrough(args) {
			t.Errorf("native command classified as gateway: %q", args)
		}
	}
	for _, args := range [][]string{nil, {"exec", "implement a function"}, {"-m", "login", "prompt"}, {"resume", "--last"}, {"--", "login"}, {"-c", `model="login"`, "exec", "prompt"}} {
		if nativePassthrough(args) {
			t.Errorf("model invocation classified as native utility: %q", args)
		}
	}
	if command, index := wrappedCommand([]string{"-C", "/tmp/project", "-m", "a/model", "exec", "resume", "--last"}); command != "exec" || index != 4 {
		t.Fatalf("global option parsing = %q at %d", command, index)
	}
	for _, args := range [][]string{{"login", "status"}, {"login", "--help"}, {"--help", "login"}, {"logout"}} {
		if nativeLoginAttempt(args) {
			t.Fatalf("read-only login utility would trigger model refresh: %q", args)
		}
	}
	if !nativeLoginAttempt([]string{"login", "--device-auth"}) {
		t.Fatal("successful native device login would not refresh models")
	}
}

func TestWrapperRecursionGuardIsLimitedToTheSameInvocation(t *testing.T) {
	a := []string{"-c", `model_provider="codex-gateway"`, "-c", `model="custom/model"`, "exec", "original prompt"}
	b := []string{"exec", "original prompt", "-c", `model="custom/model"`}
	if wrappedInvocationSignature(a) != wrappedInvocationSignature(b) {
		t.Fatal("internal gateway overrides prevented loop detection")
	}
	if wrappedInvocationSignature(a) == wrappedInvocationSignature([]string{"exec", "a different nested command"}) {
		t.Fatal("different nested Codex invocation was blocked")
	}
	if wrappedInvocationSignature(a) == wrappedInvocationSignature([]string{"-c", `model="other/model"`, "exec", "original prompt"}) {
		t.Fatal("explicitly selecting another model was treated as recursion")
	}
}

func TestWrapperResolvesUpstreamModelToConfiguredProvider(t *testing.T) {
	cfg := &Config{Models: map[string]Model{"first/gpt-test": {Provider: "first", Model: "gpt-test"}}, Providers: map[string]Provider{"first": {Auth: "api_key"}, "second": {Auth: "api_key"}, "official": {Auth: "codex"}}}
	if alias, err := resolveWrappedModel(cfg, "gpt-test"); err != nil || alias != "first/gpt-test" {
		t.Fatalf("unique upstream mapping = %q, %v", alias, err)
	}
	for _, args := range [][]string{{"-m", "gpt-test", "exec", "prompt"}, {"--model=gpt-test"}, {"-c", `model="gpt-test"`}, {"--config=model=gpt-test"}} {
		mapped := remapWrappedModelArgs(args, func(value string) string {
			if value == "gpt-test" {
				return "first/gpt-test"
			}
			return value
		})
		if selectedWrappedModel(mapped) != "first/gpt-test" {
			t.Fatalf("model flag not remapped: %q", mapped)
		}
	}
	cfg.Models["second/gpt-test"] = Model{Provider: "second", Model: "gpt-test"}
	if _, err := resolveWrappedModel(cfg, "gpt-test"); err == nil {
		t.Fatal("ambiguous third-party model silently used native Codex")
	}
	cfg.DefaultModel = "second/gpt-test"
	if alias, err := resolveWrappedModel(cfg, "gpt-test"); err != nil || alias != cfg.DefaultModel {
		t.Fatalf("current default was not preferred: %q, %v", alias, err)
	}
	cfg.Models["gpt-test"] = Model{Provider: "official", Model: "gpt-test"}
	if alias, err := resolveWrappedModel(cfg, "gpt-test"); err != nil || alias != "gpt-test" {
		t.Fatal("an explicit exact official alias was overridden")
	}
}

func TestWrapperSkipsItselfAndPreservesOriginalLauncher(t *testing.T) {
	root := t.TempDir()
	self, _ := os.Executable()
	managed, original := filepath.Join(root, "managed"), filepath.Join(root, "original")
	for _, dir := range []string{managed, original} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(self, filepath.Join(managed, "codex")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(original, "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_GATEWAY_CODEX_BIN", "")
	t.Setenv("CODEX_GATEWAY_NATIVE_CODEX_BIN", "")
	t.Setenv("PATH", managed+string(os.PathListSeparator)+original)
	if got, err := codexExecutablePath(""); err != nil || got != path {
		t.Fatalf("original launcher resolution = %q, %v", got, err)
	}
	if _, err := nativeCodexExecutablePath(); err == nil {
		t.Fatal("internal RPC would execute an unknown user shell wrapper")
	}
	t.Setenv("CODEX_GATEWAY_NATIVE_CODEX_BIN", path)
	if got, err := nativeCodexExecutablePath(); err != nil || got != path {
		t.Fatalf("explicit trusted native launcher = %q, %v", got, err)
	}
	if _, err := codexExecutablePath(filepath.Join(managed, "codex")); err == nil {
		t.Fatal("explicit recursive managed executable accepted")
	}
}

func TestWrapperRecognizesPublishedNpmNativeLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node_modules", "@openai", "codex")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"@openai/codex"}`), 0600); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "bin", "codex.js")
	if err := os.WriteFile(entry, []byte("#!/usr/bin/env node\n"), 0700); err != nil {
		t.Fatal(err)
	}
	layout := map[string][2]string{"linux/amd64": {"linux-x64", "x86_64-unknown-linux-musl"}, "linux/arm64": {"linux-arm64", "aarch64-unknown-linux-musl"}, "darwin/amd64": {"darwin-x64", "x86_64-apple-darwin"}, "darwin/arm64": {"darwin-arm64", "aarch64-apple-darwin"}}[runtime.GOOS+"/"+runtime.GOARCH]
	native := filepath.Join(root, "node_modules", "@openai", "codex-"+layout[0], "vendor", layout[1], "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("\x7fELFtest-fixture-not-executable-code"), 0700); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	if got := npmNativeExecutable(entry, self); got != native {
		t.Fatalf("npm native path = %q, want %q", got, native)
	}
}

func TestWrapperNativeInvocationNeedsNoGatewayConfiguration(t *testing.T) {
	root := t.TempDir()
	record := filepath.Join(root, "arguments")
	launcher := filepath.Join(root, "original")
	quoted := "'" + strings.ReplaceAll(record, "'", "'\\''") + "'"
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >"+quoted+"\nprintf 'original-launcher\\n'\nexit 17\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_GATEWAY_CODEX_BIN", launcher)
	t.Setenv("CODEX_GATEWAY_HOME", filepath.Join(root, "unconfigured"))
	t.Setenv("CODEX_GATEWAY_WRAPPER_ACTIVE", "")
	for _, args := range [][]string{{"--version"}, {"login", "status"}, {"resume", "--last"}, {"exec", "some prompt"}} {
		var out, errOut bytes.Buffer
		if code := executeWrapped(args, strings.NewReader(""), &out, &errOut); code != 17 || out.String() != "original-launcher\n" {
			t.Fatalf("native invocation = %d, stdout=%s stderr=%s", code, &out, &errOut)
		}
		data, err := os.ReadFile(record)
		if err != nil || string(data) != strings.Join(args, "\n")+"\n" {
			t.Fatalf("native arguments changed: %s, %v", data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "unconfigured")); !os.IsNotExist(err) {
		t.Fatal("native passthrough created gateway configuration")
	}
}

func TestWrapperOfficialOnlyRunsNativeWithoutHistoryRPC(t *testing.T) {
	home, cfg := officialTestConfig(t)
	cfg.Version, cfg.DefaultModel = 1, "official/fake-model"
	cfg.Providers = map[string]Provider{"official": {Auth: "codex", BaseURL: officialBaseURL}}
	cfg.Models = map[string]Model{"official/fake-model": {Provider: "official", Model: "fake-native-model", Template: "fake-template"}}
	if err := WriteJSON(filepath.Join(home, "config.json"), cfg); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(home, "original")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_GATEWAY_CODEX_BIN", launcher)
	t.Setenv("CODEX_GATEWAY_NATIVE_CODEX_BIN", "/must-not-run-history-helper")
	t.Setenv("CODEX_GATEWAY_HOME", home)
	t.Setenv("CODEX_GATEWAY_WRAPPER_ACTIVE", "")
	for _, test := range []struct {
		args, want []string
	}{{nil, []string{"-m", "fake-native-model"}}, {[]string{"resume", "--all"}, []string{"resume", "--all"}}, {[]string{"-m", "official/fake-model", "exec", "prompt"}, []string{"-m", "fake-native-model", "exec", "prompt"}}} {
		var out, errOut bytes.Buffer
		if code := executeWrapped(test.args, strings.NewReader(""), &out, &errOut); code != 0 || out.String() != strings.Join(test.want, "\n")+"\n" {
			t.Fatalf("official-only = %d, stdout=%q, stderr=%s", code, out.String(), &errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "runtime")); !os.IsNotExist(err) {
		t.Fatal("official-only invocation started gateway runtime")
	}
}

func TestWrapperCreatesNativeHomeOnlyForSessionOrLogin(t *testing.T) {
	home, cfg := officialTestConfig(t)
	cfg.Version = 1
	cfg.Models = map[string]Model{}
	if err := os.Remove(cfg.CodexHome); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(home, "config.json"), cfg); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(home, "original")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_GATEWAY_CODEX_BIN", launcher)
	t.Setenv("CODEX_GATEWAY_HOME", home)
	t.Setenv("CODEX_GATEWAY_WRAPPER_INVOCATION", "")
	for _, args := range [][]string{{"--version"}, {"--help"}, {"login", "status"}} {
		if code := executeWrapped(args, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
			t.Fatal("native read-only passthrough failed")
		}
		if _, err := os.Stat(cfg.CodexHome); !os.IsNotExist(err) {
			t.Fatal("read-only utility created a native home")
		}
	}
	for _, args := range [][]string{nil, {"login"}} {
		if code := executeWrapped(args, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
			t.Fatal("native first-session/login launch failed")
		}
		if info, err := os.Stat(cfg.CodexHome); err != nil || !info.IsDir() {
			t.Fatal("first native session/login did not create its selected home")
		}
		if err := os.Remove(cfg.CodexHome); err != nil {
			t.Fatal(err)
		}
	}
}

func historyResult(result, value any) error {
	data, _ := json.Marshal(value)
	return json.Unmarshal(data, result)
}

func TestWrapperResumeListsEveryProviderAndPreservesNativeID(t *testing.T) {
	cfg := &Config{Models: map[string]Model{"custom/model": {Provider: "custom"}}, Providers: map[string]Provider{"custom": {Auth: "api_key"}}}
	sessions := []wrappedSession{{ID: "old-official", ModelProvider: "openai", Model: "native-model", Preview: "old official conversation"}, {ID: "old-custom", ModelProvider: "openai", Model: "custom/model", Preview: "old gateway conversation\x1b[31m"}}
	var out bytes.Buffer
	rpc := func(_ context.Context, _ *Config, method string, params, result any) error {
		if method != "thread/list" {
			t.Fatalf("unexpected method %s", method)
		}
		p := params.(map[string]any)
		if providers, ok := p["modelProviders"].([]string); !ok || len(providers) != 0 {
			t.Fatal("resume query was restricted to the gateway provider")
		}
		if _, ok := p["cwd"]; ok {
			t.Fatal("--all did not remove directory filtering")
		}
		return historyResult(result, map[string]any{"data": sessions})
	}
	args, session, err := prepareWrappedResume([]string{"resume", "--all"}, cfg, strings.NewReader("2\n"), &out, rpc)
	if err != nil || !reflect.DeepEqual(args, []string{"resume", "old-custom", "--all"}) || session == nil || !sessionUsesGateway(*session, cfg) {
		t.Fatalf("cross-provider selection = %q, %#v, %v", args, session, err)
	}
	if !strings.Contains(out.String(), "old official conversation") || strings.Contains(out.String(), "\x1b") {
		t.Fatal("history menu omitted native sessions or emitted terminal control characters")
	}
	if sessionUsesGateway(sessions[0], cfg) {
		t.Fatal("official history would have been forced through the gateway")
	}
}

func TestWrapperResumeLastDoesNotConsumeExecPrompt(t *testing.T) {
	cfg := &Config{}
	input := strings.NewReader("original prompt from stdin")
	rpc := func(_ context.Context, _ *Config, method string, params, result any) error {
		p := params.(map[string]any)
		if _, ok := p["cwd"]; !ok || p["sourceKinds"] == nil {
			t.Fatal("exec resume directory or noninteractive history filtering missing")
		}
		return historyResult(result, map[string]any{"data": []wrappedSession{{ID: "selected-uuid", ModelProvider: "codex-gateway"}}})
	}
	args, _, err := prepareWrappedResume([]string{"exec", "resume", "--last", "--json", "-"}, cfg, input, io.Discard, rpc)
	if err != nil || !reflect.DeepEqual(args, []string{"exec", "resume", "selected-uuid", "--json", "-"}) {
		t.Fatalf("exec resume arguments = %q, %v", args, err)
	}
	if input.Len() != len("original prompt from stdin") {
		t.Fatal("--last consumed exec prompt stdin")
	}
}

func TestWrapperExplicitResumeLeavesNativeNameResolution(t *testing.T) {
	original := []string{"resume", "saved-name"}
	rpc := func(context.Context, *Config, string, any, any) error { return errors.New("native resolves names") }
	args, session, err := prepareWrappedResume(original, &Config{}, strings.NewReader(""), io.Discard, rpc)
	if err != nil || !reflect.DeepEqual(args, original) || session == nil || session.ID != "saved-name" {
		t.Fatalf("explicit resume changed: %q, %#v, %v", args, session, err)
	}
}
