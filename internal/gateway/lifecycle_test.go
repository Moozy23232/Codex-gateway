//go:build linux || darwin

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const lifecycleTestAdmin = "test-admin-token-12345678901234567890"
const lifecycleTestClient = "test-client-token-12345678901234567890"

// The production launcher re-execs its own binary. In tests that same binary
// serves an isolated temporary home or behaves as a fake Codex executable.
func TestMain(m *testing.M) {
	if os.Getenv("CODEX_GATEWAY_OFFICIAL_HELPER") == "1" {
		officialFakeCodex()
	}
	if os.Getenv("CODEX_GATEWAY_LIFECYCLE_HELPER") == "1" {
		if len(os.Args) == 4 && os.Args[1] == "--home" && os.Args[3] == "serve" {
			if os.Getenv("CODEX_GATEWAY_TEST_FAIL_START") == "1" {
				fmt.Fprintln(os.Stderr, "TEST_NOT_A_REAL_CREDENTIAL")
				os.Exit(7)
			}
			if err := Serve(os.Args[2]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
		if len(os.Args) > 1 && os.Args[1] == "-gateway-test-fake-codex" {
			lifecycleFakeCodex()
		}
		if len(os.Args) > 1 && os.Args[1] == "-gateway-test-launcher" {
			code, err := RunCodex(os.Getenv("CODEX_GATEWAY_TEST_HOME"), nil, os.Getenv("CODEX_GATEWAY_TEST_CODEX"))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(code)
		}
	}
	os.Exit(m.Run())
}

func lifecycleFakeCodex() {
	env := map[string]string{}
	for _, key := range []string{"CODEX_HOME", "CODEX_GATEWAY_TOKEN", "NO_PROXY", "no_proxy", "HTTPS_PROXY", "https_proxy"} {
		env[key] = os.Getenv(key)
	}
	if path := os.Getenv("CODEX_GATEWAY_TEST_RECORD"); path != "" {
		data, _ := json.Marshal(map[string]any{"args": os.Args[2:], "env": env})
		if err := os.WriteFile(path, data, 0600); err != nil {
			os.Exit(2)
		}
	}
	if os.Getenv("CODEX_GATEWAY_TEST_SIGNALS") == "1" {
		signals := make(chan os.Signal, 4)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		_ = os.WriteFile(os.Getenv("CODEX_GATEWAY_TEST_READY"), []byte("ready"), 0600)
		for received := range signals {
			if received == syscall.SIGTERM {
				os.Exit(17)
			}
			_ = os.WriteFile(os.Getenv("CODEX_GATEWAY_TEST_INTERRUPTED"), []byte("interrupted"), 0600)
		}
	}
	if os.Getenv("CODEX_GATEWAY_TEST_KILL_SELF") == "1" {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	code, _ := strconv.Atoi(os.Getenv("CODEX_GATEWAY_TEST_EXIT"))
	os.Exit(code)
}

type lifecycleFixture struct {
	home string
	cfg  *Config
}

func lifecycleFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	root := t.TempDir()
	home, codexHome := filepath.Join(root, "gateway"), filepath.Join(root, "codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"existing-choice\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"fake-local-login"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Version: 1, Listen: ListenConfig{Host: "127.0.0.1", Port: lifecycleFreePort(t)},
		CodexHome: codexHome, ClientAuth: "token", DefaultModel: "example/model",
		Providers: map[string]Provider{"example": {BaseURL: "https://api.example.invalid/v1", Auth: "api_key", APIKeyEnv: "GATEWAY_TEST_ONLY_KEY"}},
		Models:    map[string]Model{"example/model": {Provider: "example", Model: "upstream-model", Template: "test-template"}}}
	f := &lifecycleFixture{home: home, cfg: cfg}
	f.save(t)
	for name, value := range map[string]string{"admin-token": lifecycleTestAdmin, "client-token": lifecycleTestClient} {
		if err := PrivateWrite(filepath.Join(home, name), []byte(value+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	catalog := Catalog{Models: []map[string]any{{"slug": "example/model"}}}
	if err := WriteJSON(filepath.Join(home, "models.json"), catalog); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(home, "templates.json"), Catalog{Models: []map[string]any{{"slug": "test-template"}}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *lifecycleFixture) save(t *testing.T) {
	t.Helper()
	if err := WriteJSON(filepath.Join(f.home, "config.json"), f.cfg); err != nil {
		t.Fatal(err)
	}
}

func (f *lifecycleFixture) cleanupServer(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := Stop(f.home); err != nil {
			t.Errorf("temporary gateway cleanup: %v", err)
		}
	})
}

type lifecycleControl struct {
	server                      *httptest.Server
	mu                          sync.Mutex
	health                      map[string]any
	healthCode, stopCode, stops int
	tokens                      []string
	redirect                    string
	closeOnStop                 bool
}

func newLifecycleControl(t *testing.T, f *lifecycleFixture) *lifecycleControl {
	t.Helper()
	c := &lifecycleControl{healthCode: 200, stopCode: 200}
	c.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.tokens = append(c.tokens, r.Header.Get("X-Codex-Gateway-Token"))
		code, body := c.healthCode, any(c.health)
		if r.Method == http.MethodPost {
			c.stops++
			code, body = c.stopCode, map[string]bool{"stopping": c.stopCode == 200}
		}
		if c.redirect != "" {
			w.Header().Set("Location", c.redirect)
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
		if r.Method == http.MethodPost && code == 200 && c.closeOnStop {
			go c.server.Close()
		}
	}))
	c.server.Start()
	t.Cleanup(c.server.Close)
	u, _ := url.Parse(c.server.URL)
	port, _ := strconv.Atoi(u.Port())
	f.cfg.Listen.Port = port
	f.save(t)
	fingerprint, err := Fingerprint(f.home)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.health = map[string]any{"service": Identity, "fingerprint": fingerprint, "active_requests": 0, "pid": 12345}
	c.mu.Unlock()
	return c
}

func TestLifecycleIdentityAndRedirects(t *testing.T) {
	f := newLifecycleFixture(t)
	c := newLifecycleControl(t, f)
	status, err := Status(f.home)
	if err != nil || status["running"] != true {
		t.Fatalf("status = %v, %v", status, err)
	}
	c.mu.Lock()
	if len(c.tokens) != 1 || c.tokens[0] != lifecycleTestAdmin {
		t.Errorf("health did not authenticate")
	}
	c.health["service"] = "unrelated-service"
	c.mu.Unlock()
	for _, action := range []func(string) (map[string]any, error){Start, Stop, Status} {
		if _, err := action(f.home); err == nil {
			t.Error("unrelated service accepted")
		}
	}
	c.mu.Lock()
	c.healthCode = 403
	c.mu.Unlock()
	if _, err := Stop(f.home); err == nil || strings.Contains(err.Error(), lifecycleTestAdmin) {
		t.Errorf("wrong auth error = %v", err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed; local token exposed") }))
	defer target.Close()
	c.mu.Lock()
	c.healthCode = 302
	c.redirect = target.URL
	c.mu.Unlock()
	if _, err := Status(f.home); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Errorf("redirect error = %v", err)
	}
	c.mu.Lock()
	if c.stops != 0 {
		t.Errorf("unrecognized service received %d stop requests", c.stops)
	}
	c.mu.Unlock()
}

func TestLifecycleHealthIgnoresEnvironmentProxy(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(key, "http://127.0.0.1:1")
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	f := newLifecycleFixture(t)
	newLifecycleControl(t, f)
	if result, err := Status(f.home); err != nil || result["running"] != true {
		t.Fatalf("direct local health = %v, %v", result, err)
	}
}

func TestLifecycleBusyAndShutdownRace(t *testing.T) {
	f := newLifecycleFixture(t)
	c := newLifecycleControl(t, f)
	c.mu.Lock()
	c.health["active_requests"] = 1
	c.health["fingerprint"] = "outdated"
	c.mu.Unlock()
	for _, action := range []func(string) (map[string]any, error){Start, Stop} {
		if _, err := action(f.home); err == nil || !strings.Contains(err.Error(), "active") {
			t.Errorf("busy error = %v", err)
		}
	}
	c.mu.Lock()
	if c.stops != 0 {
		t.Errorf("busy server received stop")
	}
	c.health["active_requests"] = 0
	c.stopCode = 409
	c.mu.Unlock()
	if _, err := Stop(f.home); err == nil || !strings.Contains(err.Error(), "became busy") {
		t.Errorf("race error = %v", err)
	}
	c.mu.Lock()
	c.stopCode = 303
	c.redirect = "http://127.0.0.1:1/never"
	c.mu.Unlock()
	if _, err := Stop(f.home); err == nil || !strings.Contains(err.Error(), "HTTP 303") {
		t.Errorf("shutdown redirect error = %v", err)
	}
}

func TestLifecycleExistingServiceAndPortChange(t *testing.T) {
	f := newLifecycleFixture(t)
	old := newLifecycleControl(t, f)
	first, err := Start(f.home)
	if err != nil || first["pid"] != 12345 {
		t.Fatalf("reuse = %v, %v", first, err)
	}
	oldURL := first["url"]
	otherFixture := newLifecycleFixture(t)
	other := newLifecycleControl(t, otherFixture)
	other.mu.Lock()
	other.health["service"] = "unrelated"
	other.mu.Unlock()
	f.cfg.Listen.Port = otherFixture.cfg.Listen.Port
	f.save(t)
	status, err := Status(f.home)
	if err != nil || status["url"] != oldURL {
		t.Fatalf("old-port status = %v, %v", status, err)
	}
	if _, err := Start(f.home); err == nil {
		t.Error("occupied destination accepted")
	}
	old.mu.Lock()
	stops := old.stops
	old.closeOnStop = true
	old.mu.Unlock()
	if stops != 0 {
		t.Error("old gateway was stopped before checking new port")
	}
	if _, err := Stop(f.home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.home, "runtime", "server.json")); !errors.Is(err, os.ErrNotExist) {
		t.Error("state not removed after stop")
	}
}

func TestLifecycleRejectsUnsafeStateAndSymlinks(t *testing.T) {
	for _, entry := range []string{"runtime", "lifecycle.lock", "server.log", "server.json"} {
		t.Run(entry, func(t *testing.T) {
			f := newLifecycleFixture(t)
			runtime := filepath.Join(f.home, "runtime")
			target := filepath.Join(t.TempDir(), "untouched")
			if entry == "runtime" {
				if err := os.Mkdir(target, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, runtime); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(runtime, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("untouched"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(runtime, entry)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Start(f.home); err == nil {
				t.Error("runtime symlink accepted")
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if entry == "runtime" {
				if info.Mode().Perm() != 0755 {
					t.Error("symlink target permissions changed")
				}
			} else {
				data, _ := os.ReadFile(target)
				if string(data) != "untouched" || info.Mode().Perm() != 0644 {
					t.Error("symlink target changed")
				}
			}
		})
	}
	f := newLifecycleFixture(t)
	if err := WriteJSON(filepath.Join(f.home, "runtime", "server.json"), map[string]any{"host": "example.invalid", "port": 80, "pid": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Status(f.home); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("nonlocal state error = %v", err)
	}
}

func TestLifecycleRealSpawnReuseRestartAndStop(t *testing.T) {
	t.Setenv("CODEX_GATEWAY_LIFECYCLE_HELPER", "1")
	f := newLifecycleFixture(t)
	f.cleanupServer(t)
	first, err := Start(f.home)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Start(f.home)
	if err != nil || first["pid"] != second["pid"] {
		t.Fatalf("reuse = %v, %v", second, err)
	}
	for _, name := range []string{"server.json", "server.log", "lifecycle.lock"} {
		info, err := os.Stat(filepath.Join(f.home, "runtime", name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private runtime %s: %v", name, err)
		}
	}
	f.cfg.RetryInvalidEncryptedReasoning = true
	f.save(t)
	third, err := Start(f.home)
	if err != nil || third["pid"] == first["pid"] {
		t.Fatalf("idle restart = %v, %v", third, err)
	}
	f.cfg.Listen.Port = lifecycleFreePort(t)
	f.save(t)
	fourth, err := Start(f.home)
	if err != nil || fourth["pid"] == third["pid"] || fourth["url"] != lifecycleURL(f.cfg.Listen) {
		t.Fatalf("port restart = %v, %v", fourth, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if result, err := Stop(f.home); err != nil || result["running"] != false {
			t.Fatalf("stop = %v, %v", result, err)
		}
	}
	if result, err := Status(f.home); err != nil || result["running"] != false {
		t.Fatalf("stopped status = %v, %v", result, err)
	}
}

func TestLifecycleConcurrentStartsAndPrivateFailureLog(t *testing.T) {
	t.Setenv("CODEX_GATEWAY_LIFECYCLE_HELPER", "1")
	f := newLifecycleFixture(t)
	f.cleanupServer(t)
	type outcome struct {
		value map[string]any
		err   error
	}
	results := make(chan outcome, 2)
	for count := 0; count < 2; count++ {
		go func() { result, err := Start(f.home); results <- outcome{result, err} }()
	}
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.value["pid"] != b.value["pid"] {
		t.Fatalf("concurrent starts = %v / %v", a, b)
	}
	if _, err := Stop(f.home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_GATEWAY_TEST_FAIL_START", "1")
	if _, err := Start(f.home); err == nil || strings.Contains(err.Error(), "TEST_NOT_A_REAL_CREDENTIAL") {
		t.Fatalf("startup error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(f.home, "runtime", "server.log"))
	if err != nil || !strings.Contains(string(data), "TEST_NOT_A_REAL_CREDENTIAL") {
		t.Error("failure output was not retained in private log")
	}
}

func lifecycleEnvMap(env []string) map[string]string {
	result := map[string]string{}
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		result[key] = value
	}
	return result
}

func TestLifecycleCodexEnvironmentAndArguments(t *testing.T) {
	f := newLifecycleFixture(t)
	userArgs := []string{"exec", "prompt contains -c as ordinary text", "-c", `model="user-choice"`, "--config=reasoning.effort=\"high\"", "-cstore=false", "--", "-c", "literal"}
	args, err := lifecycleCodexArguments(f.home, f.cfg, userArgs)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd := []string{"-c", `model="user-choice"`, "-c", `reasoning.effort="high"`, "-c", "store=false", "exec", "prompt contains -c as ordinary text", "--", "-c", "literal"}
	if !reflect.DeepEqual(args[len(args)-len(wantEnd):], wantEnd) {
		t.Fatalf("argument ordering = %q", args)
	}
	if !strings.Contains(strings.Join(args, "\n"), `model_provider="codex-gateway"`) {
		t.Error("token provider missing")
	}
	for _, malformed := range [][]string{{"exec", "-c"}, {"--config", "--", "literal"}} {
		if _, err := lifecycleCodexArguments(f.home, f.cfg, malformed); err == nil {
			t.Error("missing config value accepted")
		}
	}
	env, err := lifecycleEnvironment(f.home, f.cfg, []string{"NO_PROXY=internal.invalid", "no_proxy=custom.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	values := lifecycleEnvMap(env)
	if values["CODEX_GATEWAY_TOKEN"] != lifecycleTestClient || values["CODEX_HOME"] != f.cfg.CodexHome {
		t.Error("token environment incorrect")
	}
	if values["NO_PROXY"] != "internal.invalid,custom.invalid,localhost,127.0.0.1,::1" || values["NO_PROXY"] != values["no_proxy"] {
		t.Error("loopback bypass not merged")
	}
	if _, exists := values["HTTPS_PROXY"]; exists {
		t.Error("token mode injected a bootstrap proxy")
	}
	f.cfg.ClientAuth = "codex"
	f.cfg.BootstrapProxy = "http://127.0.0.1:9191"
	env, err = lifecycleEnvironment(f.home, f.cfg, []string{"HTTPS_PROXY=http://explicit.invalid:99", "http_proxy=", "all_proxy=http://lower.invalid:80", "ALL_PROXY=http://upper.invalid:80"})
	if err != nil {
		t.Fatal(err)
	}
	values = lifecycleEnvMap(env)
	for key, want := range map[string]string{"HTTPS_PROXY": "http://explicit.invalid:99", "https_proxy": "http://explicit.invalid:99", "http_proxy": "", "HTTP_PROXY": "", "all_proxy": "http://lower.invalid:80", "ALL_PROXY": "http://upper.invalid:80"} {
		if values[key] != want {
			t.Errorf("environment %s = %q, want %q", key, values[key], want)
		}
	}
	env, err = lifecycleEnvironment(f.home, f.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycleEnvMap(env)["HTTPS_PROXY"] != f.cfg.BootstrapProxy {
		t.Error("bootstrap proxy was not supplied")
	}
	args, err = lifecycleCodexArguments(f.home, f.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, "\n"), `model_provider="openai"`) || !strings.Contains(strings.Join(args, "\n"), "openai_base_url=") {
		t.Error("native provider overrides missing")
	}
	f.cfg.DefaultModel = ""
	f.cfg.Models["a/model"] = f.cfg.Models["example/model"]
	args, err = lifecycleCodexArguments(f.home, f.cfg, nil)
	if err != nil || !strings.Contains(strings.Join(args, "\n"), `model="a/model"`) {
		t.Error("unset default is not deterministic")
	}
}

func lifecycleFakeExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "codex")
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec "+quoted+" -gateway-test-fake-codex \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLifecycleRunCodexKeepsWrapperConfigAndExitCode(t *testing.T) {
	t.Setenv("CODEX_GATEWAY_LIFECYCLE_HELPER", "1")
	t.Setenv("CODEX_GATEWAY_TEST_EXIT", "37")
	f := newLifecycleFixture(t)
	f.cleanupServer(t)
	fake := lifecycleFakeExecutable(t)
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
	record := filepath.Join(t.TempDir(), "record.json")
	t.Setenv("CODEX_GATEWAY_TEST_RECORD", record)
	beforeConfig, _ := os.ReadFile(filepath.Join(f.cfg.CodexHome, "config.toml"))
	beforeAuth, _ := os.ReadFile(filepath.Join(f.cfg.CodexHome, "auth.json"))
	code, err := RunCodex(f.home, []string{"resume", "session-id", "-c", `model="explicit"`}, "")
	if err != nil || code != 37 {
		t.Fatalf("fake Codex exit = %d, %v", code, err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Env["CODEX_GATEWAY_TOKEN"] != lifecycleTestClient || observed.Env["CODEX_HOME"] != f.cfg.CodexHome {
		t.Error("fake wrapper did not inherit gateway environment")
	}
	if strings.Contains(strings.Join(observed.Args, " "), lifecycleTestClient) || strings.Contains(strings.Join(observed.Args, " "), lifecycleTestAdmin) {
		t.Error("local token appeared in argv")
	}
	if !reflect.DeepEqual(observed.Args[len(observed.Args)-4:], []string{"-c", `model="explicit"`, "resume", "session-id"}) {
		t.Errorf("user override was not moved globally: %q", observed.Args)
	}
	afterConfig, _ := os.ReadFile(filepath.Join(f.cfg.CodexHome, "config.toml"))
	afterAuth, _ := os.ReadFile(filepath.Join(f.cfg.CodexHome, "auth.json"))
	if string(beforeConfig) != string(afterConfig) || string(beforeAuth) != string(afterAuth) {
		t.Error("existing Codex files changed")
	}
	f.cfg.ClientAuth = "codex"
	f.save(t)
	if code, err := RunCodex(f.home, nil, fake); err != nil || code != 37 {
		t.Fatalf("native-mode fake Codex = %d, %v", code, err)
	}
	data, err = os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &observed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(observed.Args, "\n"), `model_provider="openai"`) {
		t.Error("native-mode fake Codex lost its provider override")
	}
	t.Setenv("CODEX_GATEWAY_TEST_KILL_SELF", "1")
	if code, err := RunCodex(f.home, nil, fake); err != nil || code != 128+int(syscall.SIGKILL) {
		t.Fatalf("signal exit = %d, %v", code, err)
	}
}

func lifecycleWaitFile(t *testing.T, path string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("child exited before readiness: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child did not create %s", filepath.Base(path))
}

func TestLifecycleLauncherSignalsReachCodexAndWaitForWrapper(t *testing.T) {
	t.Setenv("CODEX_GATEWAY_LIFECYCLE_HELPER", "1")
	f := newLifecycleFixture(t)
	f.cleanupServer(t)
	if _, err := Start(f.home); err != nil {
		t.Fatal(err)
	}
	fake := lifecycleFakeExecutable(t)
	root := t.TempDir()
	ready, interrupted := filepath.Join(root, "ready"), filepath.Join(root, "interrupted")
	executable, _ := os.Executable()
	command := exec.Command(executable, "-gateway-test-launcher")
	command.Env = append(os.Environ(), "CODEX_GATEWAY_TEST_HOME="+f.home, "CODEX_GATEWAY_TEST_CODEX="+fake,
		"CODEX_GATEWAY_TEST_SIGNALS=1", "CODEX_GATEWAY_TEST_READY="+ready, "CODEX_GATEWAY_TEST_INTERRUPTED="+interrupted)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	log, err := os.Create(filepath.Join(root, "launcher.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	finished := false
	defer func() {
		if !finished {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
		}
	}()
	lifecycleWaitFile(t, ready, done)
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	lifecycleWaitFile(t, interrupted, done)
	select {
	case err := <-done:
		t.Fatalf("Ctrl-C detached the launcher from Codex: %v", err)
	default:
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 17 {
			t.Fatalf("launcher termination = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("launcher did not forward SIGTERM")
	}
}
