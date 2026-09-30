package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testAdminToken = "server-test-admin-credential-00001"
const testClientToken = "server-test-client-credential-0001"

type serverFixture struct {
	home    string
	gateway *gatewayServer
	server  *httptest.Server
	client  *http.Client
}

func newServerFixture(t *testing.T, upstream http.HandlerFunc, configure func(*Config)) *serverFixture {
	t.Helper()
	t.Setenv("GATEWAY_SERVER_TEST_KEY", "server-test-relay-key")
	up := httptest.NewUnstartedServer(upstream)
	up.Config.ErrorLog = log.New(io.Discard, "", 0)
	up.Start()
	t.Cleanup(up.Close)
	home := t.TempDir()
	cfg := &Config{
		Version: 1, Listen: ListenConfig{Host: "127.0.0.1", Port: 33989},
		CodexHome: filepath.Join(home, "codex"), ClientAuth: "codex",
		Providers: map[string]Provider{
			"official": {BaseURL: up.URL + "/v1", Auth: "codex"},
			"relay":    {BaseURL: up.URL + "/v1", Auth: "api_key", APIKeyEnv: "GATEWAY_SERVER_TEST_KEY"},
		},
		Models: map[string]Model{
			"gpt-test":       {Provider: "official", Model: "gpt-test", Template: "gpt-test"},
			"relay/gpt-test": {Provider: "relay", Model: "actual-model", Template: "gpt-test"},
		},
	}
	if configure != nil {
		configure(cfg)
	}
	if err := os.MkdirAll(cfg.CodexHome, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]any{
		"config.json":     cfg,
		"models.json":     Catalog{Models: []map[string]any{{"slug": "gpt-test"}, {"slug": "relay/gpt-test"}}},
		"codex/auth.json": map[string]any{"tokens": map[string]string{"access_token": "server-test-official-token"}},
	}
	for name, value := range files {
		if err := WriteJSON(filepath.Join(home, name), value); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{"admin-token": testAdminToken, "client-token": testClientToken} {
		if err := PrivateWrite(filepath.Join(home, name), []byte(value+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	g, err := newGatewayServer(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.closeIdleConnections)
	s := httptest.NewUnstartedServer(g)
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.Start()
	t.Cleanup(s.Close)
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return &serverFixture{home: home, gateway: g, server: s, client: client}
}

func (f *serverFixture) call(t *testing.T, method, path, body string, headers http.Header) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer server-test-official-token")
	for key, value := range headers {
		request.Header[key] = value
	}
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readServerResponse(t *testing.T, response *http.Response, status int) []byte {
	t.Helper()
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("status = %d, want %d; response: %s", response.StatusCode, status, data)
	}
	return data
}

type observedGatewayRequest struct {
	path   string
	header http.Header
	raw    []byte
}

func TestServerRoutesAndIsolatesCredentials(t *testing.T) {
	observed := make(chan observedGatewayRequest, 8)
	f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		observed <- observedGatewayRequest{r.URL.Path, r.Header.Clone(), raw}
		w.Header().Set("Set-Cookie", "upstream-secret=yes")
		w.Header().Set("Authorization", "Bearer upstream-private")
		w.Header().Set("Connection", "X-Upstream-Private")
		w.Header().Set("X-Upstream-Private", "private")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}, nil)
	body := `{"model":"relay/gpt-test","input":[{"type":"reasoning","id":"rs_opaque","encrypted_content":"opaque=="},{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":\"printf 中文\"}"},{"type":"function_call_output","call_id":"c1","output":"\nunchanged\n"},{"type":"compaction","encrypted_content":"summary=="}],"n":9007199254740993123456789,"decimal":0.12345678901234567890123456789,"reasoning":{"effort":"high"},"store":false}`
	headers := http.Header{
		"Chatgpt-Account-Id": {"private-account"}, "Openai-Organization": {"private-org"}, "Openai-Project": {"private-project"},
		"Cookie": {"private-cookie"}, "X-Api-Key": {"private-api-key"}, "X-Access-Token": {"private-access"},
		"X-Codex-Gateway-Token": {testAdminToken}, "Proxy-Authorization": {"private-proxy"}, "X-Custom-Credential": {"private-custom"},
		"Openai-Beta": {"responses=v1"}, "Connection": {"X-Request-ID"}, "X-Request-Id": {"connection-private"},
	}
	response := f.call(t, "POST", "/v1/responses", body, headers)
	for _, name := range []string{"Set-Cookie", "Authorization", "X-Upstream-Private"} {
		if response.Header.Get(name) != "" {
			t.Errorf("upstream private header leaked: %s", name)
		}
	}
	got := readServerResponse(t, response, 200)
	request := <-observed
	if request.path != "/v1/responses" || request.header.Get("Authorization") != "Bearer server-test-relay-key" {
		t.Fatalf("incorrect route or API credential: %s", request.path)
	}
	if request.header.Get("OpenAI-Beta") != "responses=v1" || request.header.Get("Accept-Encoding") != "identity" {
		t.Fatal("protocol headers were not preserved")
	}
	for name := range headers {
		if !strings.EqualFold(name, "OpenAI-Beta") && request.header.Get(name) != "" {
			t.Errorf("private request header leaked: %s", name)
		}
	}
	wantValue, err := decodeGatewayJSON([]byte(strings.Replace(body, `"relay/gpt-test"`, `"actual-model"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	gotValue, err := decodeGatewayJSON(got)
	if err != nil || !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatal("forwarding changed tool, reasoning, compaction, or numeric fields")
	}
	if !bytes.Contains(got, []byte("9007199254740993123456789")) || !bytes.Contains(got, []byte("0.12345678901234567890123456789")) {
		t.Fatal("JSON numbers lost precision")
	}
	for _, path := range []string{"/responses", "/v1/responses/compact", "/responses/input_tokens"} {
		readServerResponse(t, f.call(t, "POST", path, `{"model":"gpt-test"}`, http.Header{"Chatgpt-Account-Id": {"official-account"}, "Cookie": {"private"}}), 200)
		request := <-observed
		if request.path != "/v1"+strings.TrimPrefix(path, "/v1") || request.header.Get("Authorization") != "Bearer server-test-official-token" || request.header.Get("ChatGPT-Account-ID") != "official-account" || request.header.Get("Cookie") != "" {
			t.Fatal("Codex authentication passthrough or routing failed")
		}
	}
}

func TestServerRejectsMalformedAndUnsupportedRequests(t *testing.T) {
	var requests atomic.Int64
	f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(200) }, nil)
	for _, raw := range []string{`[]`, `{"model":"gpt-test","model":"other"}`, `{"model":"gpt-test","input":{"x":1,"x":2}}`, `{"model":"gpt-test","input":NaN}`, `{"model":"gpt-test","input":Infinity}`, `{"model":"gpt-test","input":1e9999}`, `{"model":"gpt-test"} {}`, "{\"model\":\"\xff\"}"} {
		readServerResponse(t, f.call(t, "POST", "/v1/responses", raw, nil), 400)
	}
	for _, test := range []struct {
		path, body string
		headers    http.Header
		status     int
	}{
		{"/v1/responses", `{"model":"missing"}`, nil, 400},
		{"/v1/chat/completions", `{"model":"gpt-test"}`, nil, 404},
		{"/v1/responses?api_key=private", `{"model":"gpt-test"}`, nil, 400},
		{"/v1/responses", `{"model":"gpt-test"}`, http.Header{"Content-Encoding": {"gzip"}}, 415},
		{"/v1/responses", `{"model":"gpt-test"}`, http.Header{"Authorization": {"Bearer server-test-official-token", "Bearer server-test-official-token"}}, 401},
		{"/v1/responses", `{"model":"gpt-test"}`, http.Header{"Authorization": {"Bearer server-test-official-token extra"}}, 401},
	} {
		readServerResponse(t, f.call(t, "POST", test.path, test.body, test.headers), test.status)
	}
	readServerResponse(t, f.call(t, "GET", "/v1/responses", "", http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}}), 426)
	readServerResponse(t, f.call(t, "GET", "/models", "", http.Header{"Authorization": {"Bearer wrong"}}), 401)
	catalog := readServerResponse(t, f.call(t, "GET", "/models", "", nil), 200)
	if !bytes.Contains(catalog, []byte(`"id":"relay/gpt-test"`)) || !bytes.Contains(catalog, []byte(`"slug":"gpt-test"`)) {
		t.Fatal("catalog aliases missing")
	}
	request, _ := http.NewRequest("POST", f.server.URL+"/responses", io.NopCloser(strings.NewReader(`{"model":"gpt-test"}`)))
	request.Header.Set("Authorization", "Bearer server-test-official-token")
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	readServerResponse(t, response, 411)
	if requests.Load() != 0 {
		t.Fatal("invalid requests reached upstream")
	}
}

func TestServerBodySizeAndReadDeadline(t *testing.T) {
	f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid body reached upstream") }, nil)
	f.gateway.requestReadTimeout = 50 * time.Millisecond
	for _, test := range []struct {
		length string
		body   string
		status int
	}{
		{"0", "", 413}, {strconv.Itoa(maxRequestBytes + 1), "", 413}, {"100", "{", 408},
	} {
		conn, err := net.DialTimeout("tcp", strings.TrimPrefix(f.server.URL, "http://"), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = fmt.Fprintf(conn, "POST /v1/responses HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer server-test-official-token\r\nContent-Length: %s\r\n\r\n%s", test.length, test.body)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		readServerResponse(t, response, test.status)
		_ = conn.Close()
	}
}

func TestServerOAuthRotationGrace(t *testing.T) {
	f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }, nil)
	var timestamp atomic.Int64
	timestamp.Store(time.Now().UnixNano())
	f.gateway.now = func() time.Time { return time.Unix(0, timestamp.Load()) }
	call := func(token string, status int) {
		readServerResponse(t, f.call(t, "POST", "/responses", `{"model":"gpt-test"}`, http.Header{"Authorization": {"Bearer " + token}}), status)
	}
	call("server-test-official-token", 200)
	if err := WriteJSON(f.gateway.authPath, map[string]any{"tokens": map[string]string{"access_token": "rotated-token"}, "OPENAI_API_KEY": "oauth-api-token"}); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"rotated-token", "oauth-api-token", "server-test-official-token"} {
		call(token, 200)
	}
	timestamp.Add(int64(authRotationGrace + time.Second))
	call("server-test-official-token", 401)
	call("rotated-token", 200)
	if err := PrivateWrite(f.gateway.authPath, []byte(`{"tokens":`)); err != nil {
		t.Fatal(err)
	}
	call("rotated-token", 200)
	timestamp.Add(int64(authRotationGrace + time.Second))
	call("rotated-token", 401)
}

func TestServerClientTokenAndFileAPIKey(t *testing.T) {
	seen := make(chan string, 1)
	f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}, func(cfg *Config) {
		cfg.ClientAuth = "token"
		delete(cfg.Providers, "official")
		delete(cfg.Models, "gpt-test")
		file := filepath.Join(t.TempDir(), "key")
		if err := PrivateWrite(file, []byte("server-test-file-key\n")); err != nil {
			t.Fatal(err)
		}
		provider := cfg.Providers["relay"]
		provider.APIKeyEnv, provider.APIKeyFile = "", file
		cfg.Providers["relay"] = provider
	})
	for _, token := range []string{"server-test-official-token", testAdminToken, "wrong"} {
		readServerResponse(t, f.call(t, "POST", "/responses", `{"model":"relay/gpt-test"}`, http.Header{"Authorization": {"Bearer " + token}}), 401)
	}
	readServerResponse(t, f.call(t, "POST", "/responses", `{"model":"relay/gpt-test"}`, http.Header{"Authorization": {"Bearer " + testClientToken}}), 200)
	if <-seen != "Bearer server-test-file-key" {
		t.Fatal("file API key was not used")
	}
}

func TestServerSSEFlushCancellationAndShutdownBusy(t *testing.T) {
	first := "event: response.created\ndata: {\"id\":\"resp_mock\"}\n\n"
	cancelled := make(chan struct{}, 1)
	f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		cancelled <- struct{}{}
	}, nil)
	admin := http.Header{"X-Codex-Gateway-Token": {testAdminToken}}
	readServerResponse(t, f.call(t, "GET", "/_gateway/health", "", nil), 403)
	readServerResponse(t, f.call(t, "POST", "/_gateway/shutdown", "{}", nil), 403)
	response := f.call(t, "POST", "/v1/responses", `{"model":"gpt-test","stream":true}`, nil)
	defer response.Body.Close()
	got := make([]byte, len(first))
	if _, err := io.ReadFull(response.Body, got); err != nil || string(got) != first {
		t.Fatalf("SSE bytes were buffered or changed: %v", err)
	}
	health := readServerResponse(t, f.call(t, "GET", "/_gateway/health", "", admin), 200)
	var state struct {
		Service     string `json:"service"`
		Active      int    `json:"active_requests"`
		PID         int    `json:"pid"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(health, &state); err != nil {
		t.Fatal(err)
	}
	if state.Service != Identity || state.Active != 1 || state.PID != os.Getpid() || state.Fingerprint != f.gateway.fingerprint {
		t.Fatalf("wrong health state: %s", health)
	}
	readServerResponse(t, f.call(t, "POST", "/_gateway/shutdown", "{}", admin), 409)
	select {
	case <-f.gateway.stop:
		t.Fatal("busy shutdown stopped server")
	default:
	}
	_ = response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not cancel upstream")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.gateway.activityMu.Lock()
		active := f.gateway.active
		f.gateway.activityMu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request count was not released")
		}
		time.Sleep(time.Millisecond)
	}
	readServerResponse(t, f.call(t, "POST", "/_gateway/shutdown", "{}", admin), 200)
	select {
	case <-f.gateway.stop:
	default:
		t.Fatal("idle shutdown did not signal stop")
	}
	readServerResponse(t, f.call(t, "POST", "/responses", `{"model":"gpt-test"}`, nil), 503)
}

func TestServerTruncatedUpstreamAbortsResponse(t *testing.T) {
	f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "{\"incomplete\":")
	}, nil)
	response := f.call(t, "POST", "/responses", `{"model":"gpt-test"}`, nil)
	defer response.Body.Close()
	_, err := io.ReadAll(response.Body)
	if err == nil {
		t.Fatal("truncated upstream appeared successfully complete")
	}
}

func TestServerSSECompletesWithOriginalBytes(t *testing.T) {
	first, last := "event: response.created\ndata: {\"text\":\"中文\"}\n\n", "data: [DONE]\n\n"
	finish := make(chan struct{})
	f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-finish:
			_, _ = io.WriteString(w, last)
		case <-r.Context().Done():
		}
	}, nil)
	response := f.call(t, "POST", "/responses", `{"model":"gpt-test","stream":true}`, nil)
	defer response.Body.Close()
	got := make([]byte, len(first))
	if _, err := io.ReadFull(response.Body, got); err != nil || string(got) != first {
		t.Fatalf("first event changed or buffered: %v", err)
	}
	close(finish)
	if got := string(readServerResponse(t, response, 200)); got != last {
		t.Fatalf("last event changed: %q", got)
	}
}

func TestServerProxyOverridesEnvironmentAndNoProxy(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, "http://127.0.0.1:1")
	}
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	for _, scheme := range []string{"direct", "http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			observed := make(chan observedGatewayRequest, 1)
			f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				observed <- observedGatewayRequest{r.Method + " " + r.RequestURI, r.Header.Clone(), nil}
				if r.Method == "CONNECT" {
					w.WriteHeader(502)
					return
				}
				_, _ = io.WriteString(w, `{}`)
			}, func(cfg *Config) {
				if scheme == "direct" {
					return
				}
				provider := cfg.Providers["relay"]
				provider.Proxy = strings.TrimSuffix(provider.BaseURL, "/v1")
				provider.BaseURL = scheme + "://provider.invalid/v1"
				provider.AllowInsecureHTTP = true
				cfg.Providers["relay"] = provider
			})
			status := 200
			if scheme == "https" {
				status = 502
			}
			readServerResponse(t, f.call(t, "POST", "/responses", `{"model":"relay/gpt-test"}`, nil), status)
			request := <-observed
			switch scheme {
			case "direct":
				if request.path != "POST /v1/responses" {
					t.Fatal("default route did not connect directly")
				}
			case "http":
				if request.path != "POST http://provider.invalid/v1/responses" {
					t.Fatal("NO_PROXY bypassed explicit HTTP proxy")
				}
			case "https":
				if request.path != "CONNECT provider.invalid:443" || request.header.Get("Authorization") != "" {
					t.Fatal("CONNECT did not use explicit proxy or leaked upstream credentials")
				}
			}
		})
	}
}

func TestServerRejectsRedirectCompressionAndMissingCredential(t *testing.T) {
	var requests atomic.Int64
	f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("redirect")) {
			w.Header().Set("Location", "http://untrusted.invalid/")
			w.WriteHeader(307)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "compressed")
	}, nil)
	readServerResponse(t, f.call(t, "POST", "/responses", `{"model":"relay/gpt-test","redirect":true}`, nil), 502)
	readServerResponse(t, f.call(t, "POST", "/responses", `{"model":"relay/gpt-test"}`, nil), 502)
	t.Setenv("GATEWAY_SERVER_TEST_KEY", "")
	readServerResponse(t, f.call(t, "POST", "/responses", `{"model":"relay/gpt-test"}`, nil), 503)
	if requests.Load() != 2 {
		t.Fatal("redirect was followed or missing credential reached upstream")
	}
}

func TestServerReasoningRetryIsNarrowAndBounded(t *testing.T) {
	for _, test := range []struct {
		name, itemType, code string
		enabled              bool
		status, wantRequests int
	}{
		{"disabled", "reasoning", "invalid_encrypted_content", false, 400, 1},
		{"enabled", "reasoning", "invalid_encrypted_content", true, 400, 2},
		{"compaction", "compaction", "invalid_encrypted_content", true, 400, 1},
		{"message", "message", "invalid_encrypted_content", true, 400, 1},
		{"unrelated", "reasoning", "rate_limit_exceeded", true, 400, 1},
		{"wrong_status", "reasoning", "invalid_encrypted_content", true, 403, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int64
			f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if bytes.Contains(raw, []byte("rs_rejected")) {
					w.WriteHeader(test.status)
					detail, _ := json.Marshal(map[string]any{"error": map[string]string{"code": test.code, "message": "The encrypted content for item rs_rejected could not be verified."}})
					wrapped, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "400", "param": string(detail)}})
					_, _ = w.Write(append([]byte("data:"), wrapped...))
					return
				}
				_, _ = w.Write(raw)
			}, func(cfg *Config) { cfg.RetryInvalidEncryptedReasoning = test.enabled })
			body := fmt.Sprintf(`{"model":"relay/gpt-test","input":[{"type":%q,"id":"rs_rejected","encrypted_content":"opaque"},{"type":"compaction","id":"rs_summary","encrypted_content":"summary"},{"type":"reasoning","id":"rs_other","encrypted_content":"valid"}]}`, test.itemType)
			status := test.status
			if test.wantRequests == 2 {
				status = 200
			}
			response := readServerResponse(t, f.call(t, "POST", "/responses", body, nil), status)
			if requests.Load() != int64(test.wantRequests) {
				t.Fatalf("retry count = %d", requests.Load())
			}
			if test.wantRequests == 2 && (!bytes.Contains(response, []byte("rs_summary")) || !bytes.Contains(response, []byte("rs_other")) || bytes.Contains(response, []byte("rs_rejected"))) {
				t.Fatal("retry removed unrelated history")
			}
			if !strings.Contains(body, "rs_rejected") {
				t.Fatal("input was mutated")
			}
		})
	}
	t.Run("limit", func(t *testing.T) {
		var requests atomic.Int64
		f := newServerFixture(t, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			raw, _ := io.ReadAll(r.Body)
			value, err := decodeGatewayJSON(raw)
			if err != nil {
				t.Error(err)
				return
			}
			input := value.(map[string]any)["input"].([]any)
			id := input[0].(map[string]any)["id"].(string)
			w.WriteHeader(400)
			_, _ = fmt.Fprintf(w, `{"error":{"code":"invalid_encrypted_content","message":"invalid encrypted content for item %s"}}`, id)
		}, func(cfg *Config) { cfg.RetryInvalidEncryptedReasoning = true })
		input := make([]map[string]any, maxCompatRetries+2)
		for i := range input {
			input[i] = map[string]any{"type": "reasoning", "id": fmt.Sprintf("rs_%d", i), "encrypted_content": "opaque"}
		}
		body, _ := json.Marshal(map[string]any{"model": "relay/gpt-test", "input": input})
		readServerResponse(t, f.call(t, "POST", "/responses", string(body), nil), 400)
		if requests.Load() != maxCompatRetries+1 {
			t.Fatalf("unbounded retry count: %d", requests.Load())
		}
	})
	for _, raw := range []string{`{"error":{"code":"invalid_encrypted_content","message":"bad payload"}}`, `{"error":{"code":"invalid_encrypted_content","message":"item msg_123"}}`, `{"error":{"code":"invalid_encrypted_content","message":"item rs_123","message":"other"}}`} {
		if id := rejectedReasoningID([]byte(raw)); id != "" {
			t.Errorf("ambiguous error was interpreted as %s", id)
		}
	}
}
