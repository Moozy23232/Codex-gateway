package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const nativeRPCTimeout = 45 * time.Second

// Codex requires an explicit CODEX_HOME to exist, including for the first
// API-key-only session. MkdirAll preserves existing directories and symlinks;
// it does not change their permissions or overwrite native files.
func ensureNativeHome(cfg *Config) error {
	if err := os.MkdirAll(cfg.CodexHome, 0700); err != nil {
		return errors.New("cannot create the selected Codex home; check its path and permissions")
	}
	return nil
}

func codexExecutablePath(explicit string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("CODEX_GATEWAY_CODEX_BIN")
	}
	if explicit == "" {
		explicit = "codex"
	}
	path, err := exec.LookPath(explicit)
	if err != nil {
		return "", errors.New("cannot find an executable Codex CLI; install Codex or set CODEX_GATEWAY_CODEX_BIN")
	}
	return path, nil
}

// These overrides apply only to this child. The user's config.toml and selected
// provider are never rewritten. File storage lets native Codex own refreshes
// while the gateway reads the resulting access token without copying it.
func nativeCodexCommand(ctx context.Context, home string, cfg *Config, args ...string) (*exec.Cmd, error) {
	executable, err := codexExecutablePath("")
	if err != nil {
		return nil, err
	}
	if err := ensureNativeHome(cfg); err != nil {
		return nil, err
	}
	nativeConfig := *cfg
	nativeConfig.ClientAuth = "codex"
	env, err := lifecycleEnvironment(home, &nativeConfig, os.Environ())
	if err != nil {
		return nil, err
	}
	filtered := make([]string, 0, len(env))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		switch key {
		case "OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_AUTH_TOKEN", "OPENAI_BASE_URL", "CHATGPT_BASE_URL", "CODEX_GATEWAY_TOKEN":
			continue
		}
		filtered = append(filtered, item)
	}
	overrides := []string{"-c", `cli_auth_credentials_store="file"`, "-c", `forced_login_method="chatgpt"`,
		"-c", `model_provider="openai"`, "-c", `chatgpt_base_url="https://chatgpt.com/backend-api"`,
		"-c", "openai_base_url=" + lifecycleJSONString(officialBaseURL)}
	command := exec.CommandContext(ctx, executable, append(overrides, args...)...)
	command.Env, command.Dir = filtered, home
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		command.Dir = os.TempDir()
	}
	command.WaitDelay = 2 * time.Second
	return command, nil
}

// nativeCodexRPC uses only the public, newline-delimited app-server protocol.
// It runs no threads or tools, and answers unexpected server requests with an
// unsupported-method error rather than granting approvals.
func nativeCodexRPC(parent context.Context, home string, cfg *Config, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(parent, nativeRPCTimeout)
	defer cancel()
	command, err := nativeCodexCommand(ctx, home, cfg, "app-server")
	if err != nil {
		return err
	}
	input, err := command.StdinPipe()
	if err != nil {
		return errors.New("cannot open native Codex input")
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return errors.New("cannot open native Codex output")
	}
	command.Stderr = io.Discard // Native diagnostics may contain account details.
	if err := command.Start(); err != nil {
		input.Close()
		output.Close()
		return errors.New("cannot start the native Codex app-server")
	}
	defer func() {
		input.Close()
		cancel()
		_ = command.Wait()
	}()
	encoder := json.NewEncoder(input)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	request := func(id int, name string, value any) error {
		return encoder.Encode(map[string]any{"id": id, "method": name, "params": value})
	}
	read := func(id int, destination any) error {
		for count := 0; count < 256 && scanner.Scan(); count++ {
			var message struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &message) != nil {
				return errors.New("native Codex returned an invalid app-server message")
			}
			if message.Method != "" {
				if len(message.ID) > 0 && string(message.ID) != "null" {
					if err := encoder.Encode(map[string]any{"id": message.ID, "error": map[string]any{"code": -32601, "message": "Unsupported by codex-gateway"}}); err != nil {
						return errors.New("native Codex app-server connection closed")
					}
				}
				continue
			}
			if string(message.ID) != fmt.Sprint(id) {
				continue
			}
			if len(message.Error) != 0 && string(message.Error) != "null" {
				return errors.New("native Codex could not complete " + method + "; check the native ChatGPT login and network connection")
			}
			if destination != nil && json.Unmarshal(message.Result, destination) != nil {
				return errors.New("native Codex returned an invalid " + method + " result")
			}
			return nil
		}
		if ctx.Err() != nil {
			return errors.New("native Codex " + method + " was canceled or timed out")
		}
		return errors.New("native Codex app-server closed before completing " + method)
	}
	if err = request(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": Identity, "version": Version}}); err != nil {
		return errors.New("cannot initialize native Codex app-server")
	}
	if err = read(1, nil); err != nil {
		return err
	}
	if err = encoder.Encode(map[string]string{"method": "initialized"}); err != nil {
		return errors.New("cannot initialize native Codex app-server")
	}
	if err = request(2, method, params); err != nil {
		return errors.New("cannot send a request to native Codex app-server")
	}
	return read(2, result)
}

type officialCredential struct {
	token     string
	accountID string
}

func readOfficialCredential(cfg *Config) (officialCredential, error) {
	file, err := os.Open(filepath.Join(cfg.CodexHome, "auth.json"))
	if err != nil {
		return officialCredential{}, errors.New("official provider needs a native ChatGPT file login; run codex-gateway add-provider --official")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxErrorBytes+1))
	if err != nil || len(raw) > maxErrorBytes {
		return officialCredential{}, errors.New("cannot read native ChatGPT credentials")
	}
	decoded, err := decodeGatewayJSON(raw)
	if err != nil {
		return officialCredential{}, errors.New("cannot read native ChatGPT credentials")
	}
	auth, ok := decoded.(map[string]any)
	if !ok {
		return officialCredential{}, errors.New("cannot read native ChatGPT credentials")
	}
	mode, _ := auth["auth_mode"].(string)
	apiKey, _ := auth["OPENAI_API_KEY"].(string)
	tokens, _ := auth["tokens"].(map[string]any)
	token, _ := tokens["access_token"].(string)
	account, _ := tokens["account_id"].(string)
	if (mode != "" && mode != "chatgpt") || apiKey != "" || !safeCredentialHeader(token) || !safeCredentialHeader(account) {
		return officialCredential{}, errors.New("official provider requires a ChatGPT subscription login, not an API key; run codex-gateway add-provider --official")
	}
	return officialCredential{token: token, accountID: account}, nil
}

func safeCredentialHeader(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c <= 32 || c >= 127 {
			return false
		}
	}
	return true
}

// EnsureOfficialLogin reuses a native file login or starts the native login UI.
// Keyring-only credentials require a fresh native file login; no token is
// extracted from the keyring or copied into gateway configuration.
func EnsureOfficialLogin(home string, cfg *Config) error {
	if _, err := readOfficialCredential(cfg); err == nil {
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	command, err := nativeCodexCommand(ctx, home, cfg, "login")
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Signing in through native Codex (ChatGPT, file credentials for this Codex home).")
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return errors.New("native Codex login did not complete; no official provider was added")
	}
	_, err = readOfficialCredential(cfg)
	return err
}

type officialAccountResult struct {
	Account *struct {
		Type string `json:"type"`
	} `json:"account"`
	WorkspaceRouting *struct {
		BackendOrigin          string `json:"backendOrigin"`
		AccountRoutingOverride string `json:"accountRoutingOverride"`
		ChatGPTAccountID       string `json:"chatgptAccountId"`
	} `json:"workspaceRouting"`
}

func (account officialAccountResult) validate(credential officialCredential) error {
	if account.Account == nil || account.Account.Type != "chatgpt" {
		return errors.New("native Codex is not signed in with ChatGPT")
	}
	if routing := account.WorkspaceRouting; routing != nil {
		parsed, err := url.Parse(routing.BackendOrigin)
		if err != nil || parsed.Scheme != "https" || parsed.Host != "chatgpt.com" || parsed.User != nil ||
			(parsed.Path != "" && parsed.Path != "/backend-api") || parsed.RawQuery != "" || parsed.Fragment != "" ||
			(routing.AccountRoutingOverride != "" && routing.AccountRoutingOverride != "NO_CONSTRAINT") {
			return errors.New("this ChatGPT workspace requires a regional backend that this gateway does not support; use native Codex for this workspace")
		}
		if routing.ChatGPTAccountID != "" && routing.ChatGPTAccountID != credential.accountID {
			return errors.New("native ChatGPT account changed during authentication; retry the request")
		}
	}
	return nil
}

type officialAuth struct {
	home      string
	config    *Config
	gate      chan struct{}
	checked   time.Time
	accountID string
	rpc       func(context.Context, string, *Config, string, any, any) error
}

func newOfficialAuth(home string, cfg *Config) *officialAuth {
	return &officialAuth{home: home, config: cfg, gate: make(chan struct{}, 1), rpc: nativeCodexRPC}
}

// Only official requests reach this helper. Native Codex refreshes on the first
// request and every 30 minutes of active official use, or once after an upstream
// 401. API-key routes never open auth.json or invoke the native app-server.
func (auth *officialAuth) credential(ctx context.Context, force bool, previousToken string) (officialCredential, error) {
	select {
	case auth.gate <- struct{}{}:
		defer func() { <-auth.gate }()
	case <-ctx.Done():
		return officialCredential{}, errors.New("official authentication was canceled")
	}
	credential, err := readOfficialCredential(auth.config)
	if err != nil {
		return officialCredential{}, err
	}
	if force && previousToken != "" && previousToken != credential.token && auth.accountID == credential.accountID {
		return credential, nil // A concurrent request already refreshed the file.
	}
	if !force && auth.accountID == credential.accountID && time.Since(auth.checked) < 30*time.Minute {
		return credential, nil
	}
	var account officialAccountResult
	if err := auth.rpc(ctx, auth.home, auth.config, "account/read", map[string]bool{"refreshToken": true}, &account); err != nil {
		return officialCredential{}, err
	}
	credential, err = readOfficialCredential(auth.config)
	if err != nil {
		return officialCredential{}, err
	}
	if err := account.validate(credential); err != nil {
		return officialCredential{}, err
	}
	auth.checked, auth.accountID = time.Now(), credential.accountID
	return credential, nil
}

func (credential officialCredential) apply(headers http.Header) {
	headers.Set("Authorization", "Bearer "+credential.token)
	headers.Set("ChatGPT-Account-ID", credential.accountID)
}
