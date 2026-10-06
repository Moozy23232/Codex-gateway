//go:build linux || darwin

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestHistoryPickerScopeAndSafety(t *testing.T) {
	for _, args := range [][]string{{"resume"}, {"fork", "--all"}, {"resume", "-m", "registered/model"}} {
		if !usesHistoryPicker(args) {
			t.Fatalf("picker rejected: %q", args)
		}
	}
	for _, args := range [][]string{{"resume", "saved-id"}, {"resume", "--last"}, {"fork", "name"}, {"exec", "resume", "--last"}, {"resume", "--help"}} {
		if usesHistoryPicker(args) {
			t.Fatalf("non-picker was intercepted: %q", args)
		}
	}
	for _, args := range [][]string{{"resume", "-s", "read-only"}, {"resume", "--sandbox=read-only"}, {"resume", "-sread-only"}, {"resume", "-a", "on-request"}, {"resume", "-p", "safe"}, {"resume", "--no-daemon"}, {"resume", "--add-dir", "/repo"}, {"resume", "-c", `sandbox_mode="read-only"`}, {"resume", "-c", `'approval_policy'="on-request"`}} {
		want := append([]string{"resume", "saved-id"}, args[1:]...)
		if got := historySelectedArguments(args, "saved-id"); !reflect.DeepEqual(got, want) {
			t.Fatalf("native permission/profile args changed: %q, want %q", got, want)
		}
	}
}

func TestHistoryPickerHonorsCD(t *testing.T) {
	for _, option := range []string{"-C", "--cd", "-C=", "--cd=", "-Cattached"} {
		t.Run(option, func(t *testing.T) {
			f := newMixedWrapperFixture(t)
			args := []string{option, f.home, "resume"}
			if strings.HasSuffix(option, "=") {
				args = []string{option + f.home, "resume"}
			} else if option == "-Cattached" {
				args = []string{"-C" + f.home, "resume"}
			}
			if code := executeWrapped(args, nil, io.Discard, io.Discard); code != 37 {
				t.Fatalf("picker exit = %d", code)
			}
			var observed struct {
				Args []string          `json:"args"`
				Env  map[string]string `json:"env"`
			}
			if err := readJSON(os.Getenv("CODEX_GATEWAY_TEST_RECORD"), &observed, true); err != nil {
				t.Fatal(err)
			}
			if len(observed.Args) < 2 || observed.Args[0] != "-C" || observed.Args[1] != f.home {
				t.Fatalf("picker cwd lost: %q", observed.Args)
			}
		})
	}
}

func TestAllProviderHistoryRequest(t *testing.T) {
	for _, params := range []string{`{}`, `null`, `{"modelProviders":null}`, `{"modelProviders":["openai"]}`, `{"modelProviders":["codex-gateway"],"cursor":"page-2","searchTerm":"name","archived":true,"cwd":"/repo","sourceKinds":["cli"]}`} {
		data := []byte(`{"id":9007199254740993,"method":"thread/list","extra":true,"params":` + params + `}`)
		got, err := allProviderHistoryRequest(data)
		if err != nil {
			t.Fatal(err)
		}
		var message map[string]json.RawMessage
		_ = json.Unmarshal(got, &message)
		if string(message["id"]) != "9007199254740993" || string(message["extra"]) != "true" {
			t.Fatalf("RPC envelope changed: %s", got)
		}
		var original, transformed map[string]json.RawMessage
		_ = json.Unmarshal([]byte(params), &original)
		_ = json.Unmarshal(message["params"], &transformed)
		if string(transformed["modelProviders"]) != "[]" {
			t.Fatalf("provider filter remains: %s", got)
		}
		delete(original, "modelProviders")
		for key, value := range original {
			if string(transformed[key]) != string(value) {
				t.Fatalf("query field %s changed: %s", key, got)
			}
		}
	}
	for _, data := range []string{`{"id":2,"method":"thread/resume","params":{"threadId":"saved"}}`, `{"id":"approval","result":{"decision":"accept"}}`, `{"method":"initialized"}`} {
		got, err := allProviderHistoryRequest([]byte(data))
		if err != nil || string(got) != data {
			t.Fatalf("non-list RPC changed: %s, %v", got, err)
		}
	}
	if _, err := allProviderHistoryRequest([]byte(`{"method":"thread/list","params":42}`)); err == nil {
		t.Fatal("invalid list parameters accepted")
	}
}

func TestHistoryBridgeSelectionDoesNotResumeBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	selected := make(chan string, 1)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		_ = bridgeNativeHistory(ctx, connection, "/bin/sh", []string{"-c", "while IFS= read -r line; do printf '%s\\n' \"$line\"; done"}, os.Environ(), selected)
	}))
	defer server.Close()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	if err := connection.Write(ctx, websocket.MessageText, []byte(`{"id":3,"method":"thread/resume","params":{"threadId":"saved-id"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := connection.Read(ctx); err == nil {
		t.Fatal("resume was forwarded to remote backend")
	}
	select {
	case id := <-selected:
		if id != "saved-id" {
			t.Fatal(id)
		}
	case <-ctx.Done():
		t.Fatal("selection lost")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("picker backend still running")
	}
}

func TestHistoryBridgeObservesExitedBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		_ = bridgeNativeHistory(ctx, connection, "/bin/sh", []string{"-c", "sleep 4 & exit 0"}, os.Environ(), nil)
	}))
	defer server.Close()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("backend exit was hidden by inherited stdout")
	}
}

func TestHistoryBridgeBidirectionalRPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		// Echo requests after an unsolicited server request, as approvals do.
		script := `printf '%s\n' '{"id":"approval","method":"item/commandExecution/requestApproval"}'; while IFS= read -r line; do printf '%s\n' "$line"; done`
		_ = bridgeNativeHistory(ctx, connection, "/bin/sh", []string{"-c", script}, os.Environ(), nil)
	}))
	defer server.Close()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	connection.SetReadLimit(1 << 20)
	_, approval, err := connection.Read(ctx)
	if err != nil || !strings.Contains(string(approval), "requestApproval") {
		t.Fatalf("server request lost: %s, %v", approval, err)
	}
	for _, message := range []string{`{"id":"approval","result":{"decision":"decline"}}`, `{"id":1,"method":"thread/list","params":{"modelProviders":["openai"]}}`, `{"id":2,"method":"thread/list","params":{"cursor":"next"}}`, `{"id":3,"result":"` + strings.Repeat("x", 40000) + `"}`} {
		if err := connection.Write(ctx, websocket.MessageText, []byte(message)); err != nil {
			t.Fatal(err)
		}
		_, got, err := connection.Read(ctx)
		want, _ := allProviderHistoryRequest([]byte(message))
		if err != nil || string(got) != string(want) {
			t.Fatalf("RPC roundtrip mismatch (%d bytes): %v", len(got), err)
		}
	}
	connection.CloseNow()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("bridge subprocess did not exit after disconnect")
	}
}
