//go:build linux || darwin

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Opt in with the same native executable as the catalog compatibility test.
// All account state, provider credentials and HTTP traffic belong to this test.
func TestNativeSetupStreamsGenericModel(t *testing.T) {
	native := os.Getenv("CODEX_GATEWAY_TEST_CODEX_BIN")
	if native == "" {
		t.Skip("set CODEX_GATEWAY_TEST_CODEX_BIN for native setup compatibility")
	}
	root := t.TempDir()
	home, nativeHome := filepath.Join(root, "gateway"), filepath.Join(root, "codex")
	binary := filepath.Join(root, "codex-gateway")
	build := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-o", binary, "../../cmd/codex-gateway")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	const reply = "native-gateway-stream-confirmed"
	const model = "native-generic-smoke"
	const key = "native-local-fake-api-key"
	var mu sync.Mutex
	var requests []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+key {
			t.Errorf("unexpected mock request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "invalid local test request", http.StatusBadRequest)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request JSON: %v", err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		sequence := 0
		send := func(kind string, payload map[string]any) {
			payload["type"], payload["sequence_number"] = kind, sequence
			sequence++
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
			w.(http.Flusher).Flush()
		}
		part := map[string]any{"type": "output_text", "text": reply, "annotations": []any{}}
		item := map[string]any{"id": "msg_native_test", "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
		response := map[string]any{"id": "resp_native_test", "object": "response", "created_at": 1, "status": "in_progress", "model": model, "output": []any{}}
		send("response.created", map[string]any{"response": response})
		send("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "msg_native_test", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
		send("response.content_part.added", map[string]any{"item_id": "msg_native_test", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		send("response.output_text.delta", map[string]any{"item_id": "msg_native_test", "output_index": 0, "content_index": 0, "delta": reply})
		send("response.output_text.done", map[string]any{"item_id": "msg_native_test", "output_index": 0, "content_index": 0, "text": reply})
		send("response.content_part.done", map[string]any{"item_id": "msg_native_test", "output_index": 0, "content_index": 0, "part": part})
		send("response.output_item.done", map[string]any{"output_index": 0, "item": item})
		response["status"], response["output"] = "completed", []any{item}
		response["usage"] = map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}
		send("response.completed", map[string]any{"response": response})
	}))
	defer upstream.Close()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root, "CODEX_HOME=" + nativeHome,
		"GATEWAY_NATIVE_TEST_KEY=" + key, "CODEX_GATEWAY_CODEX_BIN=" + native,
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9",
		"NO_PROXY=127.0.0.1,localhost", "RUST_LOG=error",
	}
	call := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, append([]string{"--home", home}, args...)...)
		command.Dir, command.Env = root, env
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
		return command.CombinedOutput()
	}
	requireCall := func(args ...string) {
		t.Helper()
		if output, err := call(args...); err != nil {
			t.Fatalf("gateway %s: %v\n%s", args[0], err, output)
		}
	}
	requireCall("add-provider", "local", "--base-url", upstream.URL+"/v1", "--api-key-env", "GATEWAY_NATIVE_TEST_KEY")
	requireCall("add-model", model)
	requireCall("config", "set", "listen.port", strconv.Itoa(lifecycleFreePort(t)))
	defer func() {
		if output, err := call("stop"); err != nil {
			t.Errorf("stop isolated gateway: %v\n%s", err, output)
		}
	}()
	lastMessage := filepath.Join(root, "last-message.txt")
	requireCall("run", "--codex-bin", native, "--", "exec", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox", "read-only", "--color", "never", "--json", "--output-last-message", lastMessage, "Return the server's reply without calling any tools.")
	text, err := os.ReadFile(lastMessage)
	if err != nil || strings.TrimSpace(string(text)) != reply {
		t.Fatalf("native client did not consume streamed response: %v, %q", err, text)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("expected one inference request, got %d", len(requests))
	}
	body := requests[0]
	if body["model"] != model || body["stream"] != true {
		t.Fatalf("native route or streaming mode changed: model=%v stream=%v", body["model"], body["stream"])
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok && (reasoning["effort"] != "none" || reasoning["summary"] != nil) {
		t.Fatalf("generic metadata enabled unsupported reasoning: %v", reasoning)
	}
	if nativeSetupContainsType(body, "input_image") || nativeSetupContainsType(body, "image_generation") {
		t.Fatal("generic text-only metadata enabled image input or generation")
	}
	reasoning, _ := json.Marshal(body["reasoning"])
	t.Logf("native setup relayed one streamed request: model=%s reasoning=%s; received %q", model, reasoning, reply)
}

func nativeSetupContainsType(value any, kind string) bool {
	switch value := value.(type) {
	case map[string]any:
		if value["type"] == kind {
			return true
		}
		for _, child := range value {
			if nativeSetupContainsType(child, kind) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if nativeSetupContainsType(child, kind) {
				return true
			}
		}
	}
	return false
}
