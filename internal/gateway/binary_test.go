package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestStandaloneBinaryWithoutLanguageRuntimes(t *testing.T) {
	if testing.Short() {
		t.Skip("standalone binary integration")
	}
	binary := filepath.Join(t.TempDir(), "codex-gateway")
	build := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-o", binary, "../../cmd/codex-gateway")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	requests := make(chan map[string]any, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.Header.Get("Authorization") != "Bearer binary-fake-secret" {
			http.Error(w, "invalid request", 400)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer upstream.Close()
	home, cfg := configFixture(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen.Port = listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	p := cfg.Providers["example"]
	p.BaseURL = upstream.URL + "/v1"
	cfg.Providers["example"] = p
	if err := SaveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	// No Go, Python, shell utilities, inherited credentials or proxy environment
	// are available to the gateway. The fake Codex's /bin/sh is explicit.
	env := []string{"PATH=/nonexistent", "HOME=" + cfg.CodexHome, "GATEWAY_TEST_KEY=binary-fake-secret"}
	call := func(args ...string) ([]byte, error) {
		cmd := exec.Command(binary, append([]string{"--home", home}, args...)...)
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	if output, err := call("--version"); err != nil || !bytes.Contains(output, []byte(Version)) {
		t.Fatalf("version: %v %s", err, output)
	}
	if output, err := call("start"); err != nil {
		t.Fatalf("start: %v %s", err, output)
	}
	defer func() {
		if output, err := call("stop"); err != nil {
			t.Errorf("stop: %v %s", err, output)
		}
	}()
	clientToken, err := ReadToken(home, "client")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(cfg.Listen.Port)+"/v1/responses", strings.NewReader(`{"model":"example/coding","input":"hello","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+clientToken)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.Contains(data, []byte("response.completed")) {
		t.Fatalf("SSE failed status=%d error=%v", response.StatusCode, err)
	}
	if body := <-requests; body["model"] != "upstream" {
		t.Fatal("binary did not route model alias")
	}
	fakeCodex := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(fakeCodex, []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = call("run", "--codex-bin", fakeCodex, "--", "exec", "hello")
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 7 {
		t.Fatalf("Codex exit code was lost: %v", err)
	}
}
