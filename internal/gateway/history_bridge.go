//go:build linux || darwin

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// Native Codex defaults missing/null modelProviders to its current provider.
// An empty array is the public app-server protocol's all-provider query.
func allProviderHistoryRequest(data []byte) ([]byte, error) {
	var message map[string]json.RawMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return nil, err
	}
	var method string
	_ = json.Unmarshal(message["method"], &method)
	if method != "thread/list" {
		return data, nil
	}
	var params map[string]json.RawMessage
	if raw := message["params"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
	}
	if params == nil {
		params = make(map[string]json.RawMessage)
	}
	params["modelProviders"] = json.RawMessage(`[]`)
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	message["params"] = encoded
	return json.Marshal(message)
}

// Only the interactive picker needs a cross-provider connection. Explicit IDs
// and --last keep native local startup, including its full CLI permissions.
func usesHistoryPicker(args []string) bool {
	command, index := wrappedCommand(args)
	return (command == "resume" || command == "fork") && !nativePassthrough(args) &&
		!nativeHasFlag(args, "--last") && len(nativePositionals(args, index+1)) == 0
}

func historyProviderArguments() []string {
	return []string{"-c", `model_provider="codex-gateway-history"`,
		"-c", `model_providers.codex-gateway-history.name="Codex history reader"`,
		"-c", `model_providers.codex-gateway-history.base_url="http://127.0.0.1:9/v1"`,
		"-c", `model_providers.codex-gateway-history.env_key="CODEX_GATEWAY_HISTORY_TOKEN"`,
		"-c", `model_providers.codex-gateway-history.wire_api="responses"`,
		"-c", `model_providers.codex-gateway-history.requires_openai_auth=false`}
}

// Keep the actual native picker, including search, paging and previews.
// The private Unix socket only bridges its WebSocket RPC to a native stdio server;
// it never edits rollout files, the history database, or provider metadata.
// A non-nil selection is replayed by the caller through its normal launch path.
func runCodexHistoryPicker(executable string, arguments, env []string) ([]string, int, error) {
	overrides, _, err := lifecycleConfigArguments(arguments)
	if err != nil {
		return nil, 1, err
	}
	var backendArgs []string
	for _, override := range overrides {
		backendArgs = append(backendArgs, "-c", override)
	}
	backendArgs = append(backendArgs, historyProviderArguments()...)
	backendArgs = append(backendArgs, "app-server", "--listen", "stdio://")
	backendEnv := replaceEnvironment(env, "CODEX_GATEWAY_HISTORY_TOKEN", "local-history-only")
	directory, err := os.MkdirTemp("", "cg-history-")
	if err != nil {
		return nil, 1, err
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "rpc.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, 1, err
	}
	selected := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	var handlers sync.WaitGroup
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		if err := bridgeNativeHistory(ctx, connection, executable, backendArgs, backendEnv, selected); err != nil && ctx.Err() == nil {
			// Do not put requests, history contents or native stderr in diagnostics.
			_ = connection.Close(websocket.StatusInternalError, "native Codex history connection ended")
		}
	})}
	go server.Serve(listener)
	defer func() {
		cancel()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
		handlers.Wait()
	}()

	command, _ := wrappedCommand(arguments)
	cwd, err := os.Getwd()
	if err != nil {
		return nil, 1, err
	}
	for i := 0; i < len(arguments) && arguments[i] != "--"; i++ {
		arg := arguments[i]
		if nativeValueOptions[arg] {
			if (arg == "-C" || arg == "--cd") && i+1 < len(arguments) {
				cwd = arguments[i+1]
			}
			i++
		} else if strings.HasPrefix(arg, "--cd=") {
			cwd = strings.TrimPrefix(arg, "--cd=")
		} else if strings.HasPrefix(arg, "-C") {
			cwd = strings.TrimPrefix(strings.TrimPrefix(arg, "-C"), "=")
		}
	}
	// Only the remote picker receives this cwd; the selected session keeps the
	// original CLI arguments and native directory prompt.
	args := []string{"-C", cwd, "--remote", "unix://" + path, command}
	if nativeHasFlag(arguments, "--all") {
		args = append(args, "--all")
	}
	if command == "resume" {
		args = append(args, "--include-non-interactive")
	}
	if nativeHasFlag(arguments, "--no-alt-screen") {
		args = append(args, "--no-alt-screen")
	}
	var diagnostics bytes.Buffer
	code, runErr := runCodexChild(executable, args, env, os.Stdin, os.Stdout, &diagnostics)
	select {
	case id := <-selected:
		cancel()
		_ = server.Close()
		// The remote path is only a picker: do not run a session through it.
		// Local native startup preserves cwd prompts, profiles and sandbox flags.
		// Closing the picker RPC produces an expected native transport error;
		// only suppress it after a confirmed selection, never on picker failure.
		return historySelectedArguments(arguments, id), 0, nil
	default:
		_, _ = io.Copy(os.Stderr, &diagnostics)
		return nil, code, runErr
	}
}

func historySelectedArguments(arguments []string, id string) []string {
	command, index := wrappedCommand(arguments)
	args := append([]string{}, arguments[:index]...)
	if id != "" {
		args = append(args, command, id)
	}
	literal := false
	for _, arg := range arguments[index+1:] {
		if arg == "--" {
			literal = true
		}
		if !literal && (arg == "--all" || arg == "--include-non-interactive") {
			continue
		}
		args = append(args, arg)
	}
	return args
}

func bridgeNativeHistory(parent context.Context, connection *websocket.Conn, executable string, args, env []string, selected chan<- string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	command.Env, command.Stderr = env, io.Discard
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 2 * time.Second
	input, err := command.StdinPipe()
	if err != nil {
		return err
	}
	defer input.Close()
	output, writer := io.Pipe()
	command.Stdout = writer
	defer output.Close()
	defer writer.Close()
	if err := command.Start(); err != nil {
		return err
	}
	waited := make(chan error, 1)
	go func() {
		err := command.Wait()
		writer.Close()
		waited <- err
	}()
	// ponytail-lite: bound individual RPC messages at 64 MiB; raise this only
	// if a real large-history/image workload exceeds it, not by dropping turns.
	const maxMessage = 64 << 20
	connection.SetReadLimit(maxMessage)
	done := make(chan error, 2)
	go func() {
		err := func() error {
			encoder := json.NewEncoder(input)
			for {
				kind, data, err := connection.Read(ctx)
				if err != nil {
					return err
				}
				if kind != websocket.MessageText {
					return errors.New("expected a text app-server message")
				}
				if selected != nil {
					var request struct {
						Method string `json:"method"`
						Params struct {
							ThreadID string `json:"threadId"`
						} `json:"params"`
					}
					if err := json.Unmarshal(data, &request); err != nil {
						return err
					}
					if request.Method == "thread/resume" || request.Method == "thread/fork" || request.Method == "thread/start" {
						if request.Method != "thread/start" && request.Params.ThreadID == "" {
							return errors.New("missing selected thread ID")
						}
						select {
						case selected <- request.Params.ThreadID:
						case <-ctx.Done():
						}
						return nil // Close the picker before any session is created or resumed.
					}
				}
				data, err = allProviderHistoryRequest(data)
				if err != nil {
					return err
				}
				if err := encoder.Encode(json.RawMessage(data)); err != nil {
					return err
				}
			}
		}()
		done <- err
	}()
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), maxMessage)
		for scanner.Scan() {
			if err := connection.Write(ctx, websocket.MessageText, scanner.Bytes()); err != nil {
				done <- err
				return
			}
		}
		done <- scanner.Err()
	}()
	remaining := 2
	select {
	case err = <-done:
		remaining--
	case err = <-waited:
		waited = nil
	}
	cancel()
	input.Close()
	output.Close()
	for ; remaining > 0; remaining-- {
		<-done
	}
	if waited != nil {
		<-waited
	}
	return err
}
