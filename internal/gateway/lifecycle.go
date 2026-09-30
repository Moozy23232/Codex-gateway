//go:build linux || darwin

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	lifecycleStartTimeout = 10 * time.Second
	lifecycleStopTimeout  = 5 * time.Second
	lifecyclePollInterval = 100 * time.Millisecond
	lifecycleMaxBody      = 64 * 1024
)

type lifecycleState struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	PID         int    `json:"pid"`
	Fingerprint string `json:"fingerprint"`
}

type lifecycleHealth struct {
	Service        string `json:"service"`
	Fingerprint    string `json:"fingerprint"`
	ActiveRequests *int   `json:"active_requests"`
	PID            *int   `json:"pid"`
}

func lifecycleEndpoint(endpoint ListenConfig) (string, error) {
	ip := net.ParseIP(endpoint.Host)
	if (endpoint.Host != "localhost" && (ip == nil || !ip.IsLoopback())) || endpoint.Port < 1 || endpoint.Port > 65535 {
		return "", errors.New("gateway runtime contains an invalid loopback address")
	}
	return net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.Port)), nil
}

func lifecycleURL(endpoint ListenConfig) string {
	return "http://" + net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.Port))
}

// Runtime entries must never redirect writes through a symlink. A gateway home
// itself can be a user-selected symlink, as with an existing mounted Codex home.
func lifecycleRuntime(home string, create bool) (string, error) {
	path := filepath.Join(home, "runtime")
	if create {
		if err := os.MkdirAll(path, 0700); err != nil {
			return "", err
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !create {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("gateway runtime must be a directory, not a symlink")
	}
	if create {
		if err := os.Chmod(path, 0700); err != nil {
			return "", err
		}
	}
	return path, nil
}

func lifecycleOpen(path string, flags int, private bool) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("gateway runtime entry must be a regular file")
	}
	if private {
		if err := file.Chmod(0600); err != nil {
			file.Close()
			return nil, err
		}
	}
	return file, nil
}

func lifecycleLock(home string) (func(), error) {
	runtime, err := lifecycleRuntime(home, true)
	if err != nil {
		return nil, err
	}
	file, err := lifecycleOpen(filepath.Join(runtime, "lifecycle.lock"), syscall.O_RDWR|syscall.O_CREAT, true)
	if err != nil {
		return nil, fmt.Errorf("cannot open private gateway lifecycle lock: %w", err)
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}

func lifecycleReadState(home string) (*lifecycleState, error) {
	runtime, err := lifecycleRuntime(home, false)
	if err != nil {
		return nil, err
	}
	file, err := lifecycleOpen(filepath.Join(runtime, "server.json"), syscall.O_RDONLY, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot read gateway runtime/server.json; check the local runtime state")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, lifecycleMaxBody+1))
	var value lifecycleState
	if err != nil || len(data) > lifecycleMaxBody || json.Unmarshal(data, &value) != nil {
		return nil, errors.New("gateway runtime/server.json must contain a valid object")
	}
	if _, err := lifecycleEndpoint(ListenConfig{Host: value.Host, Port: value.Port}); err != nil {
		return nil, err
	}
	return &value, nil
}

func lifecycleWriteState(home string, endpoint ListenConfig, health *lifecycleHealth) error {
	runtime, err := lifecycleRuntime(home, true)
	if err != nil {
		return err
	}
	path := filepath.Join(runtime, "server.json")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("gateway runtime/server.json must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return WriteJSON(path, lifecycleState{Host: endpoint.Host, Port: endpoint.Port,
		PID: *health.PID, Fingerprint: health.Fingerprint})
}

func lifecycleClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true,
			DialContext: (&net.Dialer{Timeout: timeout}).DialContext},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func lifecycleCheckHealth(endpoint ListenConfig, token string) (*lifecycleHealth, error) {
	if _, err := lifecycleEndpoint(endpoint); err != nil {
		return nil, err
	}
	address := lifecycleURL(endpoint)
	request, err := http.NewRequest(http.MethodGet, address+"/_gateway/health", nil)
	if err != nil {
		return nil, errors.New("invalid gateway health endpoint")
	}
	request.Header.Set("X-Codex-Gateway-Token", token)
	client := lifecycleClient(time.Second)
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, nil
		}
		return nil, fmt.Errorf("the service at %s did not answer the gateway health check; leaving it unchanged", address)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cannot verify ownership of the service at %s (HTTP %d); leaving it unchanged", address, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, lifecycleMaxBody+1))
	var value lifecycleHealth
	if err != nil || len(data) > lifecycleMaxBody || json.Unmarshal(data, &value) != nil ||
		value.Service != Identity || value.Fingerprint == "" || value.ActiveRequests == nil ||
		*value.ActiveRequests < 0 || value.PID == nil || *value.PID <= 0 {
		return nil, fmt.Errorf("the service at %s is not this gateway; leaving it unchanged", address)
	}
	return &value, nil
}

func lifecycleLocate(home string, cfg *Config, token string) (ListenConfig, *lifecycleHealth, error) {
	desired := cfg.Listen
	if _, err := lifecycleEndpoint(desired); err != nil {
		return desired, nil, err
	}
	previous, err := lifecycleReadState(home)
	if err != nil {
		return desired, nil, err
	}
	if previous != nil {
		recorded := ListenConfig{Host: previous.Host, Port: previous.Port}
		health, err := lifecycleCheckHealth(recorded, token)
		if err != nil {
			return recorded, nil, err
		}
		if health != nil || recorded == desired {
			return recorded, health, nil
		}
	}
	health, err := lifecycleCheckHealth(desired, token)
	return desired, health, err
}

func lifecycleResult(endpoint ListenConfig, health *lifecycleHealth) map[string]any {
	if health == nil {
		return map[string]any{"running": false}
	}
	return map[string]any{"running": true, "service": health.Service, "fingerprint": health.Fingerprint,
		"active_requests": *health.ActiveRequests, "pid": *health.PID, "url": lifecycleURL(endpoint)}
}

// Status reports only an authenticated gateway; a saved PID is never trusted.
func Status(home string) (map[string]any, error) {
	home, err := ResolvePath(home)
	if err != nil {
		return nil, err
	}
	cfg, err := LoadConfig(home)
	if err != nil {
		return nil, err
	}
	token, err := ReadToken(home, "admin")
	if err != nil {
		return nil, err
	}
	endpoint, health, err := lifecycleLocate(home, cfg, token)
	if err != nil {
		return nil, err
	}
	return lifecycleResult(endpoint, health), nil
}

func lifecycleShutdown(endpoint ListenConfig, token string, health *lifecycleHealth) error {
	if *health.ActiveRequests != 0 {
		return errors.New("gateway has active requests; let them finish before stopping or restarting it")
	}
	request, err := http.NewRequest(http.MethodPost, lifecycleURL(endpoint)+"/_gateway/shutdown", http.NoBody)
	if err != nil {
		return errors.New("invalid gateway shutdown endpoint")
	}
	request.Header.Set("X-Codex-Gateway-Token", token)
	client := lifecycleClient(2 * time.Second)
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return errors.New("gateway did not confirm shutdown; no process was killed")
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, lifecycleMaxBody))
	response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return errors.New("gateway became busy; let active requests finish before stopping or restarting it")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway refused shutdown (HTTP %d); no process was killed", response.StatusCode)
	}
	if readErr != nil {
		return errors.New("gateway did not confirm shutdown; no process was killed")
	}
	address, err := lifecycleEndpoint(endpoint)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(lifecycleStopTimeout)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil
		}
		if err == nil {
			connection.Close()
		}
		time.Sleep(lifecyclePollInterval)
	}
	return errors.New("gateway did not stop within five seconds; no process was killed")
}

// Stop requests shutdown only after authenticated health confirms an idle server.
func Stop(home string) (map[string]any, error) {
	home, err := ResolvePath(home)
	if err != nil {
		return nil, err
	}
	unlock, err := lifecycleLock(home)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cfg, err := LoadConfig(home)
	if err != nil {
		return nil, err
	}
	token, err := ReadToken(home, "admin")
	if err != nil {
		return nil, err
	}
	endpoint, health, err := lifecycleLocate(home, cfg, token)
	if err != nil {
		return nil, err
	}
	if health != nil {
		if err := lifecycleShutdown(endpoint, token, health); err != nil {
			return nil, err
		}
	}
	if err := os.Remove(filepath.Join(home, "runtime", "server.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return map[string]any{"running": false}, nil
}

func lifecycleCancelSpawn(command *exec.Cmd, done <-chan error) {
	// This process handle belongs to the child just created. Never signal a PID
	// loaded from runtime/server.json: a restart may have reused that PID.
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		return
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

// Start reuses a matching server or replaces this home's idle stale server.
func Start(home string) (map[string]any, error) {
	home, err := ResolvePath(home)
	if err != nil {
		return nil, err
	}
	unlock, err := lifecycleLock(home)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cfg, err := LoadConfig(home)
	if err != nil {
		return nil, err
	}
	token, err := ReadToken(home, "admin")
	if err != nil {
		return nil, err
	}
	fingerprint, err := Fingerprint(home)
	if err != nil {
		return nil, err
	}
	desired := cfg.Listen
	endpoint, health, err := lifecycleLocate(home, cfg, token)
	if err != nil {
		return nil, err
	}
	if health != nil {
		if endpoint == desired && health.Fingerprint == fingerprint {
			if err := lifecycleWriteState(home, endpoint, health); err != nil {
				return nil, err
			}
			return lifecycleResult(endpoint, health), nil
		}
		if *health.ActiveRequests != 0 {
			return nil, errors.New("gateway configuration changed while requests are active; retry after they finish")
		}
		if endpoint != desired {
			other, err := lifecycleCheckHealth(desired, token)
			if err != nil {
				return nil, err
			}
			if other != nil {
				return nil, errors.New("the new gateway port is already occupied; the existing gateway was left running")
			}
		}
		if err := lifecycleShutdown(endpoint, token, health); err != nil {
			return nil, err
		}
	}
	runtime, err := lifecycleRuntime(home, true)
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(runtime, "server.log")
	log, err := lifecycleOpen(logPath, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_APPEND, true)
	if err != nil {
		return nil, fmt.Errorf("cannot open private gateway log: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		log.Close()
		return nil, errors.New("cannot locate the gateway executable")
	}
	command := exec.Command(executable, "--home", home, "serve")
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = command.Start()
	log.Close()
	if err != nil {
		return nil, fmt.Errorf("could not launch the gateway; check the executable and %s", logPath)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ready := false
	defer func() {
		if !ready {
			lifecycleCancelSpawn(command, done)
		}
	}()
	deadline := time.Now().Add(lifecycleStartTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return nil, fmt.Errorf("gateway exited during startup; inspect %s locally for details", logPath)
		default:
		}
		health, err = lifecycleCheckHealth(desired, token)
		if err != nil {
			return nil, err
		}
		if health != nil {
			if health.Fingerprint != fingerprint || *health.PID != command.Process.Pid {
				return nil, errors.New("another process claimed the gateway port during startup; leaving that service unchanged")
			}
			if err := lifecycleWriteState(home, desired, health); err != nil {
				return nil, err
			}
			ready = true
			return lifecycleResult(desired, health), nil
		}
		time.Sleep(lifecyclePollInterval)
	}
	return nil, fmt.Errorf("gateway startup timed out; inspect %s locally for details", logPath)
}

func lifecycleConfigArguments(args []string) ([]string, []string, error) {
	var overrides, remaining []string
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			remaining = append(remaining, args[index:]...)
			break
		}
		if argument == "-c" || argument == "--config" {
			if index+1 == len(args) || args[index+1] == "--" {
				return nil, nil, fmt.Errorf("%s requires a key=value argument before --", argument)
			}
			index++
			overrides = append(overrides, args[index])
		} else if strings.HasPrefix(argument, "--config=") {
			overrides = append(overrides, strings.TrimPrefix(argument, "--config="))
		} else if strings.HasPrefix(argument, "-c") && !strings.HasPrefix(argument, "--") {
			overrides = append(overrides, strings.TrimPrefix(strings.TrimPrefix(argument, "-c"), "="))
		} else {
			remaining = append(remaining, argument)
		}
	}
	return overrides, remaining, nil
}

func lifecycleEnvironment(home string, cfg *Config, inherited []string) ([]string, error) {
	env := map[string]string{}
	for _, item := range inherited {
		if key, value, ok := strings.Cut(item, "="); ok {
			env[key] = value
		}
	}
	env["CODEX_HOME"] = cfg.CodexHome
	bypass, seen := []string{}, map[string]bool{}
	for _, item := range strings.Split(env["NO_PROXY"]+","+env["no_proxy"]+",localhost,127.0.0.1,::1,"+cfg.Listen.Host, ",") {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			bypass = append(bypass, item)
			seen[item] = true
		}
	}
	env["NO_PROXY"], env["no_proxy"] = strings.Join(bypass, ","), strings.Join(bypass, ",")
	if cfg.ClientAuth == "token" {
		token, err := ReadToken(home, "client")
		if err != nil {
			return nil, err
		}
		env["CODEX_GATEWAY_TOKEN"] = token
	} else {
		proxy, exists := env["all_proxy"]
		if !exists {
			proxy, exists = env["ALL_PROXY"]
		}
		if !exists && cfg.BootstrapProxy != "" {
			proxy, exists = cfg.BootstrapProxy, true
		}
		if !exists {
			names := make([]string, 0, len(cfg.Providers))
			for name := range cfg.Providers {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				provider := cfg.Providers[name]
				if provider.Auth == "codex" && provider.Proxy != "" {
					proxy, exists = provider.Proxy, true
					break
				}
			}
		}
		if exists {
			for _, lower := range []string{"http_proxy", "https_proxy", "all_proxy"} {
				upper := strings.ToUpper(lower)
				lowerValue, lowerExists := env[lower]
				upperValue, upperExists := env[upper]
				switch {
				case !lowerExists && !upperExists:
					env[lower], env[upper] = proxy, proxy
				case !lowerExists:
					env[lower] = upperValue
				case !upperExists:
					env[upper] = lowerValue
				}
			}
		}
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+env[key])
	}
	return result, nil
}

func lifecycleJSONString(value string) string { data, _ := json.Marshal(value); return string(data) }

func lifecycleCodexArguments(home string, cfg *Config, userArgs []string) ([]string, error) {
	model := cfg.DefaultModel
	if model == "" {
		models := make([]string, 0, len(cfg.Models))
		for alias := range cfg.Models {
			models = append(models, alias)
		}
		sort.Strings(models)
		if len(models) == 0 {
			return nil, errors.New("no gateway models are configured; run codex-gateway add-provider, then codex-gateway add-model")
		}
		model = models[0]
	}
	userOverrides, remaining, err := lifecycleConfigArguments(userArgs)
	if err != nil {
		return nil, err
	}
	overrides := []string{"model_catalog_json=" + lifecycleJSONString(filepath.Join(home, "models.json")), "features.enable_request_compression=false"}
	baseURL := lifecycleJSONString(lifecycleURL(cfg.Listen) + "/v1")
	if cfg.ClientAuth == "codex" {
		overrides = append(overrides, `model_provider="openai"`, "openai_base_url="+baseURL)
	} else {
		overrides = append(overrides, `model_provider="codex-gateway"`, `model_providers.codex-gateway.name="Codex Gateway"`,
			"model_providers.codex-gateway.base_url="+baseURL, `model_providers.codex-gateway.wire_api="responses"`,
			`model_providers.codex-gateway.env_key="CODEX_GATEWAY_TOKEN"`, "model_providers.codex-gateway.requires_openai_auth=false")
	}
	overrides = append(overrides, "model="+lifecycleJSONString(model))
	overrides = append(overrides, userOverrides...)
	command := make([]string, 0, 2*len(overrides)+len(remaining))
	for _, override := range overrides {
		command = append(command, "-c", override)
	}
	return append(command, remaining...), nil
}

// RunCodex preserves the installed Codex wrapper and changes only this child's
// environment and arguments. User -c options follow defaults at the global level.
func RunCodex(home string, args []string, codexExecutable string) (int, error) {
	home, err := ResolvePath(home)
	if err != nil {
		return 1, err
	}
	if _, err := os.Stat(filepath.Join(home, "config.json")); errors.Is(err, os.ErrNotExist) {
		return 1, errors.New("gateway is not configured; run codex-gateway add-provider, then codex-gateway add-model")
	}
	cfg, err := LoadConfig(home)
	if err != nil {
		return 1, err
	}
	arguments, err := lifecycleCodexArguments(home, cfg, args)
	if err != nil {
		return 1, err
	}
	executable, err := codexExecutablePath(codexExecutable)
	if err != nil {
		return 1, err
	}
	env, err := lifecycleEnvironment(home, cfg, os.Environ())
	if err != nil {
		return 1, err
	}
	if err := ensureNativeHome(cfg); err != nil {
		return 1, err
	}
	if _, err := Start(home); err != nil {
		return 1, err
	}
	return runCodexChild(executable, arguments, env, os.Stdin, os.Stdout, os.Stderr)
}

func runCodexChild(executable string, arguments, env []string, in io.Reader, out, errOut io.Writer) (int, error) {
	env = replaceEnvironment(env, "CODEX_GATEWAY_WRAPPER_INVOCATION", wrappedInvocationSignature(arguments))
	command := exec.Command(executable, arguments...)
	command.Env = env
	command.Stdin, command.Stdout, command.Stderr = in, out, errOut
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		return 1, errors.New("unable to start Codex; check its executable and permissions")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	for {
		select {
		case received := <-signals:
			// The terminal delivers Ctrl-C to Codex and its wrapper in our shared
			// process group. Continue waiting so the wrapper can finish backups.
			if received == syscall.SIGTERM {
				_ = command.Process.Signal(syscall.SIGTERM)
			}
		case err := <-done:
			if err == nil {
				return 0, nil
			}
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					return 128 + int(status.Signal()), nil
				}
				return exitError.ExitCode(), nil
			}
			return 1, errors.New("could not wait for the Codex process")
		}
	}
}
