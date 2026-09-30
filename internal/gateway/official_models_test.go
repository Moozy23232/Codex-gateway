package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func officialModelFixture(t *testing.T) (string, *Config, officialModelSnapshot) {
	t.Helper()
	home, cfg := emptyModelSetupHome(t)
	cfg.Providers["official"] = Provider{BaseURL: officialBaseURL, Auth: "codex", AutoModels: true}
	officialTestCredential(t, cfg, "model-test-old-token")
	snapshot := officialModelSnapshot{
		Models: []map[string]any{nativeTestModel(t, "gpt-6.1-sol"), nativeTestModel(t, "gpt-6-sol")},
		Choices: []nativeModelChoice{
			{ID: "gpt-6.1-sol", Model: "gpt-6.1-sol", DisplayName: "GPT-6.1-Sol", IsDefault: true},
			{ID: "gpt-6-sol", Model: "gpt-6-sol", DisplayName: "GPT-6-Sol"},
		},
		ClientVersion: bundledNativeModelsVersion,
		AccountScope:  officialModelAccountScope(cfg, "fake-account"),
		FetchedAt:     time.Now().UTC(),
	}
	return home, cfg, snapshot
}

func TestOfficialModelsSynchronizePreserveManualAndRemoveStaleManaged(t *testing.T) {
	home, cfg, snapshot := officialModelFixture(t)
	cfg.Providers["third"] = Provider{BaseURL: "https://api.example.invalid/v1", Auth: "api_key", APIKeyEnv: "LOCAL_TEST_KEY"}
	manual := nativeTestModel(t, "gpt-6.1-sol")
	manual["slug"] = "manual-template"
	if err := WriteJSON(filepath.Join(home, "templates.json"), Catalog{Models: []map[string]any{manual}}); err != nil {
		t.Fatal(err)
	}
	cfg.Models["gpt-6.1-sol"] = Model{Provider: "third", Model: "custom-name", Template: "manual-template"}
	cfg.DefaultModel = "gpt-6.1-sol"
	load := func(context.Context, string, *Config, Provider) (officialModelSnapshot, error) { return snapshot, nil }
	report, err := syncOfficialModels(context.Background(), home, cfg, load)
	if err != nil || report.Added != 2 || report.Updated != 0 || report.Removed != 0 {
		t.Fatalf("initial sync: %+v %v", report, err)
	}
	if !cfg.Models["official/gpt-6.1-sol"].Managed || !cfg.Models["gpt-6-sol"].Managed || cfg.Models["gpt-6.1-sol"].Provider != "third" || cfg.DefaultModel != "gpt-6.1-sol" {
		t.Fatal("official sync replaced a manual route or default")
	}
	if err := SaveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	report, err = syncOfficialModels(context.Background(), home, cfg, load)
	if err != nil || report.Added != 0 || report.Updated != 0 || report.Removed != 0 {
		t.Fatalf("repeated sync changed routes: %+v %v", report, err)
	}
	oldTemplate := cfg.Models["gpt-6-sol"].Template
	cfg.DefaultModel = "gpt-6-sol"
	snapshot.Choices = snapshot.Choices[:1]
	report, err = syncOfficialModels(context.Background(), home, cfg, load)
	if err != nil || report.Removed != 1 || cfg.DefaultModel != "official/gpt-6.1-sol" {
		t.Fatalf("removed model/default was not refreshed: %+v %v", report, err)
	}
	if _, ok := cfg.Models["gpt-6-sol"]; ok || cfg.Models["gpt-6.1-sol"].Provider != "third" {
		t.Fatal("sync kept a stale managed model or removed a manual model")
	}
	templates, _ := TemplatesFrom(filepath.Join(home, "templates.json"))
	if findModelTemplate(templates, oldTemplate) == nil {
		t.Fatal("sync deleted historical template data")
	}
}

func TestOfficialModelsOfflineCacheIsAccountScopedAndFailureIsAtomic(t *testing.T) {
	home, cfg, snapshot := officialModelFixture(t)
	load := func(context.Context, string, *Config, Provider) (officialModelSnapshot, error) { return snapshot, nil }
	if _, err := syncOfficialModels(context.Background(), home, cfg, load); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(cfg)
	templatesBefore, _ := os.ReadFile(filepath.Join(home, "templates.json"))
	cacheBefore, _ := os.ReadFile(filepath.Join(home, "official-models-cache.json"))
	offline := func(context.Context, string, *Config, Provider) (officialModelSnapshot, error) {
		return officialModelSnapshot{}, errors.New("offline")
	}
	report, err := syncOfficialModels(context.Background(), home, cfg, offline)
	if err != nil || !report.Cached {
		t.Fatalf("same-account offline data was not preserved: %+v %v", report, err)
	}
	if err := WriteJSON(filepath.Join(cfg.CodexHome, "auth.json"), map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"access_token": "other-fake-token", "account_id": "other-account"}}); err != nil {
		t.Fatal(err)
	}
	if report, err := syncOfficialModels(context.Background(), home, cfg, offline); err == nil || report.Cached {
		t.Fatal("previous account cache was presented as the new account's model list")
	}
	snapshot.Choices = append(snapshot.Choices, nativeModelChoice{ID: "gpt-missing-exact-info", Model: "gpt-missing-exact-info"})
	if _, err := syncOfficialModels(context.Background(), home, cfg, load); err == nil {
		t.Fatal("incomplete native capability metadata was accepted")
	}
	after, _ := json.Marshal(cfg)
	templatesAfter, _ := os.ReadFile(filepath.Join(home, "templates.json"))
	cacheAfter, _ := os.ReadFile(filepath.Join(home, "official-models-cache.json"))
	if !bytes.Equal(before, after) || !bytes.Equal(templatesBefore, templatesAfter) || !bytes.Equal(cacheBefore, cacheAfter) {
		t.Fatal("failed sync left partial configuration or cache changes")
	}
}

type officialModelsTransport func(*http.Request) (*http.Response, error)

func (transport officialModelsTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestOfficialModelFetchUsesFixedEndpointAndBoundedNativeRefresh(t *testing.T) {
	home, cfg, snapshot := officialModelFixture(t)
	auth := newOfficialAuth(home, cfg)
	refreshes := 0
	auth.rpc = func(_ context.Context, _ string, _ *Config, method string, _ any, result any) error {
		refreshes++
		if method != "account/read" {
			t.Fatalf("unexpected native method %s", method)
		}
		officialTestCredential(t, cfg, "model-test-refreshed-token")
		return json.Unmarshal([]byte(`{"account":{"type":"chatgpt"}}`), result)
	}
	credential, err := readOfficialCredential(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client, err := newUpstreamClient("")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	data, _ := json.Marshal(Catalog{Models: snapshot.Models})
	client.Transport = officialModelsTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.String() != officialBaseURL+"/models?client_version=0.159.2" || request.Method != http.MethodGet || request.Header.Get("ChatGPT-Account-ID") != "fake-account" {
			t.Fatal("official model request used an unexpected endpoint/account")
		}
		for _, key := range []string{"Cookie", "X-Codex-Gateway-Token", "Proxy-Authorization", "OpenAI-Organization"} {
			if request.Header.Get(key) != "" {
				t.Errorf("unrelated credential leaked through %s", key)
			}
		}
		status, body, token := http.StatusOK, string(data), "model-test-refreshed-token"
		if requests == 1 {
			status, body, token = http.StatusUnauthorized, "private diagnostic never echo", "model-test-old-token"
		}
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("model request did not use the current native credential")
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	catalog, err := fetchOfficialModelCatalog(context.Background(), client, auth, credential, bundledNativeModelsVersion)
	if err != nil || len(catalog.Models) != 2 || requests != 2 || refreshes != 1 {
		t.Fatalf("model discovery retry: requests=%d refreshes=%d error=%v", requests, refreshes, err)
	}
}

func TestOfficialModelFetchRejectsRedirectsLargeBodiesAndPrivateErrors(t *testing.T) {
	home, cfg, _ := officialModelFixture(t)
	credential, _ := readOfficialCredential(cfg)
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"redirect", http.StatusFound, "DO_NOT_ECHO_PRIVATE_RESPONSE"},
		{"server error", http.StatusInternalServerError, "DO_NOT_ECHO_PRIVATE_RESPONSE"},
		{"too large", http.StatusOK, strings.Repeat(" ", modelListMaxBytes+1)},
		{"invalid JSON", http.StatusOK, `{"models":[],"models":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := newUpstreamClient("")
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			client.Transport = officialModelsTransport(func(request *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: test.status, Header: http.Header{"Location": []string{"https://example.invalid/private"}}, Body: io.NopCloser(strings.NewReader(test.body)), Request: request}, nil
			})
			_, err = fetchOfficialModelCatalog(context.Background(), client, newOfficialAuth(home, cfg), credential, bundledNativeModelsVersion)
			if err == nil || requests != 1 || strings.Contains(err.Error(), "DO_NOT_ECHO") {
				t.Fatalf("unsafe model discovery failure: requests=%d error=%v", requests, err)
			}
		})
	}
}

func TestNativeOfficialModelPickerIgnoresUserAliasCatalog(t *testing.T) {
	native := os.Getenv("CODEX_GATEWAY_TEST_CODEX_BIN")
	if native == "" {
		t.Skip("set CODEX_GATEWAY_TEST_CODEX_BIN for native model picker compatibility")
	}
	home, cfg := emptyModelSetupHome(t)
	t.Setenv("CODEX_GATEWAY_NATIVE_CODEX_BIN", native)
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "http://127.0.0.1:9")
	}
	fake := nativeTestModel(t, "gpt-6.1-sol")
	fake["slug"] = "user-gateway-alias"
	aliasPath := filepath.Join(t.TempDir(), "aliases.json")
	if err := WriteJSON(aliasPath, Catalog{Models: []map[string]any{fake}}); err != nil {
		t.Fatal(err)
	}
	config := []byte("model_catalog_json = " + lifecycleJSONString(aliasPath) + "\n")
	if err := PrivateWrite(filepath.Join(cfg.CodexHome, "config.toml"), config); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	choices, err := nativeOfficialModelChoices(ctx, home, cfg, Catalog{Models: []map[string]any{nativeTestModel(t, "gpt-6.1-sol")}})
	if err != nil || len(choices) != 1 || choices[0].Model != "gpt-6.1-sol" {
		t.Fatalf("native picker loaded a user alias catalog: %#v %v", choices, err)
	}
	after, err := os.ReadFile(filepath.Join(cfg.CodexHome, "config.toml"))
	if err != nil || !bytes.Equal(after, config) {
		t.Fatal("native picker rewrote user configuration")
	}
	if temporary, _ := filepath.Glob(filepath.Join(home, ".official-models-*.json")); len(temporary) != 0 {
		t.Fatal("scoped native model catalog was not cleaned up")
	}
}
