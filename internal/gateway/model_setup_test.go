package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func emptyModelSetupHome(t *testing.T) (string, *Config) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "gateway")
	cfg, err := Initialize(home, InitOptions{AuthMode: "token", CodexHome: filepath.Join(root, "codex")})
	if err != nil {
		t.Fatal(err)
	}
	return home, cfg
}

func TestPrepareGenericTemplatePreservesExistingRoutes(t *testing.T) {
	home, cfg := configFixture(t)
	original, err := LoadCatalog(home)
	if err != nil {
		t.Fatal(err)
	}
	slug, err := PrepareModelTemplate(home, cfg, "unlisted-model", ModelTemplateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := TemplatesFrom(filepath.Join(home, "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	item := findModelTemplate(catalog, slug)
	if !reflect.DeepEqual(item["input_modalities"], []any{"text"}) || len(item["supported_reasoning_levels"].([]any)) != 0 {
		t.Fatal("generic metadata advertised unsupported capabilities")
	}
	for _, name := range []string{"support_verbosity", "supports_parallel_tool_calls", "supports_search_tool", "supports_reasoning_summaries", "supports_reasoning_summary_parameter", "prefer_websockets"} {
		if item[name] != false {
			t.Fatalf("generic metadata enabled %s", name)
		}
	}
	if item["context_window"] != json.Number("32000") || item["auto_compact_token_limit"] != json.Number("28000") {
		t.Fatal("generic context budget missing")
	}
	cfg.Models["example/unlisted"] = Model{Provider: "example", Model: "unlisted-model", Template: slug}
	if err := SaveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	now, _ := LoadCatalog(home)
	if !reflect.DeepEqual(now.Models[0], original.Models[0]) {
		t.Fatal("adding a generic template changed existing model metadata")
	}
	before, _ := os.ReadFile(filepath.Join(home, "templates.json"))
	again, err := PrepareModelTemplate(home, cfg, "unlisted-model", ModelTemplateOptions{})
	after, _ := os.ReadFile(filepath.Join(home, "templates.json"))
	if err != nil || again != slug || !bytes.Equal(before, after) {
		t.Fatalf("template generation is not idempotent: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, "templates.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("generated template file must remain private")
	}
}

func TestPrepareExactTemplateAndIndependentOverrides(t *testing.T) {
	home, cfg := emptyModelSetupHome(t)
	known := map[string]any{
		"slug": "known-model", "context_window": 128000,
		"supported_reasoning_levels": []any{map[string]any{"effort": "high", "description": "High"}},
		"nested":                     map[string]any{"precise": json.Number("9007199254740993")},
	}
	if err := WriteJSON(filepath.Join(cfg.CodexHome, "models_cache.json"), Catalog{Models: []map[string]any{known}}); err != nil {
		t.Fatal(err)
	}
	initial, err := PrepareModelTemplate(home, cfg, "known-model", ModelTemplateOptions{})
	if err != nil || initial != "known-model" {
		t.Fatalf("exact cache match not imported: %q, %v", initial, err)
	}
	variant, err := PrepareModelTemplate(home, cfg, "known-model", ModelTemplateOptions{ContextWindow: 64000, ReasoningEffort: "high"})
	if err != nil || variant == initial {
		t.Fatalf("override did not get a separate template: %v", err)
	}
	catalog, _ := TemplatesFrom(filepath.Join(home, "templates.json"))
	original := findModelTemplate(catalog, initial)
	changed := findModelTemplate(catalog, variant)
	if original["context_window"] != json.Number("128000") || changed["context_window"] != json.Number("64000") || changed["max_context_window"] != json.Number("64000") || changed["auto_compact_token_limit"] != json.Number("56000") {
		t.Fatal("context overrides changed the shared source or kept an incompatible context limit")
	}
	if changed["nested"].(map[string]any)["precise"] != json.Number("9007199254740993") {
		t.Fatal("copying metadata lost integer precision")
	}
	if changed["default_reasoning_level"] != "high" {
		t.Fatal("reasoning default not updated")
	}
	explicit, err := PrepareModelTemplate(home, cfg, "another-model", ModelTemplateOptions{Template: initial})
	if err != nil || explicit != initial {
		t.Fatal("explicit template selection failed")
	}
	unknown, err := PrepareModelTemplate(home, cfg, "known-model-suffix", ModelTemplateOptions{})
	if err != nil || unknown == initial || unknown == variant {
		t.Fatal("non-exact model name inherited known model capabilities")
	}
}

func TestPrepareTemplateRejectsOptionsWithoutWriting(t *testing.T) {
	home, cfg := configFixture(t)
	before, _ := os.ReadFile(filepath.Join(home, "templates.json"))
	for _, tc := range []struct {
		model string
		opts  ModelTemplateOptions
	}{
		{"", ModelTemplateOptions{}},
		{"model\x1b", ModelTemplateOptions{}},
		{"model", ModelTemplateOptions{ContextWindow: -1}},
		{"model", ModelTemplateOptions{ContextWindow: 1_000_000_001}},
		{"model", ModelTemplateOptions{ReasoningEffort: "high\n"}},
		{"model", ModelTemplateOptions{Template: "missing"}},
		{"model", ModelTemplateOptions{Template: "template", ReasoningEffort: "high"}},
	} {
		if _, err := PrepareModelTemplate(home, cfg, tc.model, tc.opts); err == nil {
			t.Errorf("accepted invalid options: %#v", tc)
		}
		after, _ := os.ReadFile(filepath.Join(home, "templates.json"))
		if !bytes.Equal(before, after) {
			t.Fatal("invalid template setup changed disk state")
		}
	}
	slug, err := PrepareModelTemplate(home, cfg, "new-reasoner", ModelTemplateOptions{ReasoningEffort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := TemplatesFrom(filepath.Join(home, "templates.json"))
	item := findModelTemplate(catalog, slug)
	if !templateSupportsEffort(item, "high") || item["default_reasoning_level"] != "high" || item["supports_reasoning_summaries"] != false {
		t.Fatal("explicit reasoning option enabled unrelated capabilities or was not retained")
	}
}

func TestParseProviderModelsDefendsSelectionOutput(t *testing.T) {
	choices, err := parseProviderModels([]byte(`{"data":[{"id":"z","display_name":"Z model"},{"id":"a","display_name":"bad\u001b[2J"},{"id":"z"},{"id":"bad\nmodel"},{"id":"model/large"}]}`))
	want := []ModelChoice{{ID: "a", DisplayName: "a"}, {ID: "model/large", DisplayName: "model/large"}, {ID: "z", DisplayName: "Z model"}}
	if err != nil || !reflect.DeepEqual(choices, want) {
		t.Fatalf("choices = %#v, error = %v", choices, err)
	}
	for _, data := range []string{`{}`, `{"data":null}`, `{"data":[]}`, `{"data":[{"id":123}]}`, `{"data":[],"data":[]}`, `{"data":[{"id":"a","id":"b"}]}`, `{"data":[{"id":" "}]}`} {
		if _, err := parseProviderModels([]byte(data)); err == nil {
			t.Errorf("accepted invalid model list %s", data)
		}
	}
}

func TestDiscoverProviderModelsUsesPrivateKeyAndDirectConnection(t *testing.T) {
	home, cfg := emptyModelSetupHome(t)
	const key = "local-model-discovery-key"
	t.Setenv("GATEWAY_MODEL_TEST_KEY", key)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/models" || r.Header.Get("Authorization") != "Bearer "+key || r.Header.Get("Accept") != "application/json" {
			t.Error("unexpected model discovery request")
		}
		for _, name := range []string{"Cookie", "ChatGPT-Account-Id", "X-Codex-Gateway-Token", "Proxy-Authorization"} {
			if r.Header.Get(name) != "" {
				t.Errorf("discovery leaked %s", name)
			}
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"available-model"}]}`)
	}))
	defer server.Close()
	cfg.Providers["example"] = Provider{BaseURL: server.URL + "/api/v1/", Auth: "api_key", APIKeyEnv: "GATEWAY_MODEL_TEST_KEY"}
	choices, err := DiscoverProviderModels(home, cfg, "example")
	if err != nil || len(choices) != 1 || choices[0].ID != "available-model" || requests.Load() != 1 {
		t.Fatalf("model discovery failed: %#v %v", choices, err)
	}
}

func TestDiscoverProviderModelsRefusesRedirectsAndHidesErrors(t *testing.T) {
	home, cfg := emptyModelSetupHome(t)
	const secret = "test-secret-do-not-echo"
	t.Setenv("GATEWAY_MODEL_TEST_KEY", secret)
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/"+secret, http.StatusFound)
	}))
	defer server.Close()
	cfg.Providers["example"] = Provider{BaseURL: server.URL, Auth: "api_key", APIKeyEnv: "GATEWAY_MODEL_TEST_KEY"}
	_, err := DiscoverProviderModels(home, cfg, "example")
	if err == nil || !strings.Contains(err.Error(), "302") || strings.Contains(err.Error(), secret) || redirected.Load() != 0 {
		t.Fatalf("redirect followed or credential exposed: %v", err)
	}
}

func TestDiscoverProviderModelsEnforcesExplicitProxyAndBodyLimit(t *testing.T) {
	home, cfg := emptyModelSetupHome(t)
	t.Setenv("GATEWAY_MODEL_TEST_KEY", "local-model-test")
	t.Setenv("NO_PROXY", "*")
	var large atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "upstream.example.invalid" || r.URL.Path != "/v1/models" {
			t.Error("explicit proxy did not receive target URL")
		}
		if large.Load() {
			fmt.Fprint(w, strings.Repeat(" ", modelListMaxBytes+1))
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"proxy-model"}]}`)
	}))
	defer proxy.Close()
	cfg.Providers["example"] = Provider{BaseURL: "http://upstream.example.invalid/v1", Auth: "api_key", APIKeyEnv: "GATEWAY_MODEL_TEST_KEY", Proxy: proxy.URL, AllowInsecureHTTP: true}
	choices, err := DiscoverProviderModels(home, cfg, "example")
	if err != nil || len(choices) != 1 || choices[0].ID != "proxy-model" {
		t.Fatalf("proxy not honored: %#v %v", choices, err)
	}
	large.Store(true)
	if _, err := DiscoverProviderModels(home, cfg, "example"); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized list accepted: %v", err)
	}
}

// Opt in with a real native Codex binary. This uses an isolated home, a local
// catalog and a dummy custom provider; it performs no inference or account login.
func TestModelTemplateNativeCatalog(t *testing.T) {
	binary := os.Getenv("CODEX_GATEWAY_TEST_CODEX_BIN")
	if binary == "" {
		t.Skip("set CODEX_GATEWAY_TEST_CODEX_BIN for native catalog compatibility")
	}
	home, cfg := emptyModelSetupHome(t)
	slug, err := PrepareModelTemplate(home, cfg, "native-generic-check", ModelTemplateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["test"] = Provider{BaseURL: "http://127.0.0.1:9/v1", Auth: "api_key", APIKeyEnv: "GATEWAY_MODEL_TEST_KEY"}
	cfg.Models["test/generic"] = Model{Provider: "test", Model: "native-generic-check", Template: slug}
	if err := SaveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	nativeHome := filepath.Join(root, "codex")
	if err := os.MkdirAll(nativeHome, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	options := []string{
		`model_provider="gateway_test"`, `model="test/generic"`,
		`model_catalog_json=` + fmt.Sprintf("%q", filepath.Join(home, "models.json")),
		`cli_auth_credentials_store="file"`,
		`model_providers.gateway_test.name="Test"`,
		`model_providers.gateway_test.base_url="http://127.0.0.1:9/v1"`,
		`model_providers.gateway_test.wire_api="responses"`,
		`model_providers.gateway_test.requires_openai_auth=false`,
		`model_providers.gateway_test.env_key="GATEWAY_MODEL_TEST_KEY"`,
	}
	args := []string{}
	for _, option := range options {
		args = append(args, "-c", option)
	}
	args = append(args, "app-server")
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = root
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root, "CODEX_HOME=" + nativeHome,
		"GATEWAY_MODEL_TEST_KEY=local-dummy", "HTTP_PROXY=http://127.0.0.1:9",
		"HTTPS_PROXY=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9", "NO_PROXY=", "RUST_LOG=error",
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	in, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); command.Process.Kill(); command.Wait() }()
	fmt.Fprintln(in, `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"gateway-catalog-check","version":"0.0.0"},"capabilities":{"experimentalApi":true}}}`)
	decoder := json.NewDecoder(out)
	for {
		var message struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := decoder.Decode(&message); err != nil {
			t.Fatalf("native catalog check failed: %v (stderr: %s)", err, stderr.String())
		}
		if len(message.Error) != 0 {
			t.Fatalf("native catalog check returned %s", message.Error)
		}
		if message.ID == 1 {
			fmt.Fprintln(in, `{"method":"initialized","params":{}}`)
			fmt.Fprintln(in, `{"id":2,"method":"model/list","params":{"includeHidden":true}}`)
		}
		if message.ID != 2 {
			continue
		}
		var response struct {
			Data []struct {
				ID                     string   `json:"id"`
				InputModalities        []string `json:"inputModalities"`
				SupportedReasoning     []any    `json:"supportedReasoningEfforts"`
				DefaultReasoningEffort string   `json:"defaultReasoningEffort"`
			} `json:"data"`
		}
		if err := json.Unmarshal(message.Result, &response); err != nil || len(response.Data) != 1 {
			t.Fatalf("native model list did not use generated metadata: %s", message.Result)
		}
		model := response.Data[0]
		if model.ID != "test/generic" || !reflect.DeepEqual(model.InputModalities, []string{"text"}) || len(model.SupportedReasoning) != 0 || model.DefaultReasoningEffort != "none" {
			t.Fatalf("native model list capabilities changed: %s", message.Result)
		}
		return
	}
}
