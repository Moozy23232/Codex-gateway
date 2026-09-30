package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func officialFakeCodex() {
	if path := os.Getenv("CODEX_GATEWAY_OFFICIAL_RECORD"); path != "" {
		env := map[string]string{}
		for _, key := range []string{"CODEX_HOME", "OPENAI_API_KEY", "CODEX_AUTH_TOKEN", "OPENAI_BASE_URL", "CODEX_GATEWAY_TOKEN", "HTTPS_PROXY", "https_proxy"} {
			env[key] = os.Getenv(key)
		}
		data, _ := json.Marshal(map[string]any{"args": os.Args[1:], "env": env})
		_ = os.WriteFile(path, data, 0600)
	}
	if os.Args[len(os.Args)-1] == "login" {
		_ = os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"fake-login-token","account_id":"fake-account"}}`), 0600)
		os.Exit(0)
	}
	if os.Getenv("CODEX_GATEWAY_OFFICIAL_MODE") == "hang" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	initialized, acknowledged := false, false
	for scanner.Scan() {
		var message struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			os.Exit(3)
		}
		switch message.Method {
		case "initialize":
			initialized = true
			_ = encoder.Encode(map[string]any{"id": message.ID, "result": map[string]any{}})
		case "initialized":
			acknowledged = initialized
		case "account/read":
			if !acknowledged || message.Params["refreshToken"] != true {
				os.Exit(4)
			}
			if os.Getenv("CODEX_GATEWAY_OFFICIAL_MODE") == "error" {
				_ = encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32603, "message": "DO_NOT_PRINT_SECRET_ACCOUNT_DETAIL"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"method": "account/updated", "params": map[string]any{}})
			_ = encoder.Encode(map[string]any{"id": message.ID, "result": map[string]any{"account": map[string]any{"type": "chatgpt"}}})
		default:
			os.Exit(5)
		}
	}
	os.Exit(0)
}

func officialTestConfig(t *testing.T) (string, *Config) {
	t.Helper()
	home := t.TempDir()
	cfg := &Config{CodexHome: filepath.Join(home, "codex"), ClientAuth: "token",
		Listen: ListenConfig{Host: "127.0.0.1", Port: 33989}, Providers: map[string]Provider{}}
	if err := os.MkdirAll(cfg.CodexHome, 0700); err != nil {
		t.Fatal(err)
	}
	return home, cfg
}

func officialTestCredential(t *testing.T, cfg *Config, token string) {
	t.Helper()
	if err := WriteJSON(filepath.Join(cfg.CodexHome, "auth.json"), map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"access_token": token, "account_id": "fake-account"}}); err != nil {
		t.Fatal(err)
	}
}

func officialTestExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "native-codex")
	quoted := "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec "+quoted+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_GATEWAY_CODEX_BIN", launcher)
	t.Setenv("CODEX_GATEWAY_OFFICIAL_HELPER", "1")
	return launcher
}

func TestOfficialNativeProtocolAndChildConfiguration(t *testing.T) {
	home, cfg := officialTestConfig(t)
	officialTestExecutable(t)
	path := filepath.Join(home, "record.json")
	t.Setenv("CODEX_GATEWAY_OFFICIAL_RECORD", path)
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_AUTH_TOKEN", "OPENAI_BASE_URL", "CODEX_GATEWAY_TOKEN"} {
		t.Setenv(key, "unrelated-private-value")
	}
	cfg.BootstrapProxy = "http://127.0.0.1:6789"
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		old, exists := os.LookupEnv(key)
		os.Unsetenv(key)
		t.Cleanup(func() {
			if exists {
				os.Setenv(key, old)
			} else {
				os.Unsetenv(key)
			}
		})
	}
	var result officialAccountResult
	if err := nativeCodexRPC(context.Background(), home, cfg, "account/read", map[string]bool{"refreshToken": true}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Account == nil || result.Account.Type != "chatgpt" {
		t.Fatal("account result was not decoded")
	}
	var record struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &record) != nil {
		t.Fatal("cannot read fake Codex record")
	}
	for _, setting := range []string{`cli_auth_credentials_store="file"`, `forced_login_method="chatgpt"`, `model_provider="openai"`} {
		if !strings.Contains(strings.Join(record.Args, "\n"), setting) {
			t.Errorf("missing native override %s", setting)
		}
	}
	if record.Env["CODEX_HOME"] != cfg.CodexHome || record.Env["HTTPS_PROXY"] != cfg.BootstrapProxy {
		t.Fatal("native home or bootstrap proxy was not isolated")
	}
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_AUTH_TOKEN", "OPENAI_BASE_URL", "CODEX_GATEWAY_TOKEN"} {
		if record.Env[key] != "" {
			t.Errorf("unrelated credential or endpoint leaked through %s", key)
		}
	}
}

func TestOfficialNativeCancellationAndPrivateErrors(t *testing.T) {
	home, cfg := officialTestConfig(t)
	officialTestExecutable(t)
	t.Setenv("CODEX_GATEWAY_OFFICIAL_MODE", "error")
	err := nativeCodexRPC(context.Background(), home, cfg, "account/read", map[string]bool{"refreshToken": true}, &officialAccountResult{})
	if err == nil || strings.Contains(err.Error(), "DO_NOT_PRINT") {
		t.Fatalf("unsafe or missing native error: %v", err)
	}
	t.Setenv("CODEX_GATEWAY_OFFICIAL_MODE", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = nativeCodexRPC(ctx, home, cfg, "account/read", map[string]bool{"refreshToken": true}, &officialAccountResult{})
	if err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("native cancellation not bounded: %v", err)
	}
}

func TestOfficialLoginReusesFileAndUsesNativeForMissingLogin(t *testing.T) {
	home, cfg := officialTestConfig(t)
	officialTestCredential(t, cfg, "existing-fake-token")
	t.Setenv("CODEX_GATEWAY_CODEX_BIN", filepath.Join(home, "must-not-be-run"))
	if err := EnsureOfficialLogin(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cfg.CodexHome, "auth.json")); err != nil {
		t.Fatal(err)
	}
	// A first-time Codex user has neither a native home nor an auth file.
	cfg.CodexHome = filepath.Join(home, "first-native-home")
	officialTestExecutable(t)
	if err := EnsureOfficialLogin(filepath.Join(home, "not-yet-initialized-gateway"), cfg); err != nil {
		t.Fatal(err)
	}
	credential, err := readOfficialCredential(cfg)
	if err != nil || credential.token != "fake-login-token" {
		t.Fatal("native file login was not used")
	}
	if _, err := os.Stat(filepath.Join(cfg.CodexHome, "config.toml")); !os.IsNotExist(err) {
		t.Fatal("login wrote global native configuration")
	}
}

func TestOfficialCredentialWaitHonorsCancellation(t *testing.T) {
	home, cfg := officialTestConfig(t)
	auth := newOfficialAuth(home, cfg)
	auth.gate <- struct{}{}
	defer func() { <-auth.gate }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := auth.credential(ctx, false, ""); err == nil || time.Since(start) > time.Second {
		t.Fatal("waiting behind another refresh ignored request cancellation")
	}
}

func TestOfficialRejectsAPIKeyAndRoutingConstraints(t *testing.T) {
	_, cfg := officialTestConfig(t)
	for _, value := range []string{`{"OPENAI_API_KEY":"fake-api-key"}`, `{"tokens":{"access_token":"fake\nheader","account_id":"account"}}`, `{"tokens":{"access_token":"fake","account_id":""}}`} {
		if err := PrivateWrite(filepath.Join(cfg.CodexHome, "auth.json"), []byte(value)); err != nil {
			t.Fatal(err)
		}
		if _, err := readOfficialCredential(cfg); err == nil {
			t.Fatal("non-ChatGPT or unsafe credential accepted")
		}
	}
	for _, data := range []string{
		`{"account":{"type":"chatgpt"},"workspaceRouting":{"backendOrigin":"https://example.invalid","accountRoutingOverride":"NO_CONSTRAINT"}}`,
		`{"account":{"type":"chatgpt"},"workspaceRouting":{"backendOrigin":"https://chatgpt.com","accountRoutingOverride":"us"}}`,
		`{"account":{"type":"chatgpt"},"workspaceRouting":{"backendOrigin":"https://chatgpt.com","accountRoutingOverride":"NO_CONSTRAINT","chatgptAccountId":"another-account"}}`,
		`{"account":{"type":"apiKey"}}`,
	} {
		var account officialAccountResult
		_ = json.Unmarshal([]byte(data), &account)
		if err := account.validate(officialCredential{accountID: "fake-account"}); err == nil {
			t.Fatalf("unsafe native routing accepted: %s", data)
		}
	}
}

type officialRoundTrip func(*http.Request) (*http.Response, error)

func (fn officialRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestOfficialTokenGatewayIsolationRefreshAndRetry(t *testing.T) {
	home, cfg := officialTestConfig(t)
	t.Setenv("GATEWAY_OFFICIAL_TEST_API_KEY", "fake-third-party-key")
	cfg.Providers = map[string]Provider{
		"official": {Auth: "codex", BaseURL: officialBaseURL},
		"custom":   {Auth: "api_key", BaseURL: "https://example.invalid/v1", APIKeyEnv: "GATEWAY_OFFICIAL_TEST_API_KEY"},
	}
	cfg.Models = map[string]Model{
		"official/model": {Provider: "official", Model: "actual-official"},
		"custom/model":   {Provider: "custom", Model: "actual-custom"},
	}
	auth := newOfficialAuth(home, cfg)
	refreshes := 0
	auth.rpc = func(_ context.Context, _ string, _ *Config, method string, params, result any) error {
		if method != "account/read" || params.(map[string]bool)["refreshToken"] != true {
			t.Fatal("refresh was not delegated to native Codex")
		}
		refreshes++
		officialTestCredential(t, cfg, fmt.Sprintf("native-token-%d", refreshes))
		return json.Unmarshal([]byte(`{"account":{"type":"chatgpt"}}`), result)
	}
	upstreamCalls := 0
	responseCodes := []int{401, 200}
	g := &gatewayServer{config: cfg, official: auth, clientToken: "fake-gateway-token", requestReadTimeout: time.Second,
		clients: map[string]*http.Client{}, stop: make(chan struct{})}
	g.clients["custom"] = &http.Client{Transport: officialRoundTrip(func(r *http.Request) (*http.Response, error) {
		if refreshes != 0 || r.Header.Get("Authorization") != "Bearer fake-third-party-key" || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Fatal("third-party request touched or leaked official credentials")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}
	g.clients["official"] = &http.Client{Transport: officialRoundTrip(func(r *http.Request) (*http.Response, error) {
		upstreamCalls++
		if r.URL.String() != officialBaseURL+"/responses" || r.Header.Get("Authorization") != fmt.Sprintf("Bearer native-token-%d", refreshes) ||
			r.Header.Get("ChatGPT-Account-ID") != "fake-account" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Custom-Credential") != "" {
			t.Fatal("official request leaked client headers or used incorrect native credentials")
		}
		code := responseCodes[min(upstreamCalls-1, len(responseCodes)-1)]
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}
	call := func(alias string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"model":%q}`, alias)
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		r.Header.Set("Content-Length", fmt.Sprint(len(body)))
		r.Header.Set("Authorization", "Bearer fake-gateway-token")
		r.Header.Set("ChatGPT-Account-ID", "untrusted-client-account")
		r.Header.Set("Cookie", "untrusted-cookie")
		r.Header.Set("X-Custom-Credential", "untrusted-secret")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w
	}
	if response := call("custom/model"); response.Code != 200 {
		t.Fatalf("third-party route required a native login: %d %s", response.Code, response.Body)
	}
	if response := call("official/model"); response.Code != 503 || refreshes != 0 || upstreamCalls != 0 {
		t.Fatal("missing native login was not stopped before upstream access")
	}
	officialTestCredential(t, cfg, "old-native-token")
	if response := call("official/model"); response.Code != 200 || refreshes != 2 || upstreamCalls != 2 {
		t.Fatalf("official retry = %d, refreshes %d, upstream calls %d: %s", response.Code, refreshes, upstreamCalls, response.Body)
	}
	responseCodes, upstreamCalls = []int{401, 401}, 0
	if response := call("official/model"); response.Code != 401 || refreshes != 3 || upstreamCalls != 2 {
		t.Fatal("official 401 retried more than once")
	}
}

func TestCodexExecutableEnvironmentAndExplicitPrecedence(t *testing.T) {
	current := officialTestExecutable(t)
	if actual, err := codexExecutablePath(""); err != nil || actual != current {
		t.Fatalf("environment executable = %q, %v", actual, err)
	}
	t.Setenv("CODEX_GATEWAY_CODEX_BIN", "/nonexistent/fake-codex")
	if actual, err := codexExecutablePath(current); err != nil || actual != current {
		t.Fatalf("explicit executable = %q, %v", actual, err)
	}
}

func TestRunCodexUnconfiguredHomeGuidance(t *testing.T) {
	home := filepath.Join(t.TempDir(), "unconfigured")
	code, err := RunCodex(home, nil, "/must-not-be-executed")
	if code != 1 || err == nil || !strings.Contains(err.Error(), "add-provider") || !strings.Contains(err.Error(), "add-model") {
		t.Fatalf("unconfigured home guidance = %d, %v", code, err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("bare launch created a home before setup")
	}
}
