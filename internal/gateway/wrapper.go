package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/pelletier/go-toml/v2"
)

const installManifestName = "codex-gateway.install.json"

type installManifest struct {
	Version            int    `json:"version"`
	OriginalExecutable string `json:"original_executable,omitempty"`
	NativeExecutable   string `json:"native_executable,omitempty"`
}

func readInstallManifest(directory string) (*installManifest, error) {
	path := filepath.Join(directory, installManifestName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("cannot read the gateway installation manifest")
	}
	var manifest installManifest
	if err := readJSON(path, &manifest, true); err != nil || manifest.Version != 1 {
		return nil, errors.New("invalid gateway installation manifest")
	}
	for _, executable := range []string{manifest.OriginalExecutable, manifest.NativeExecutable} {
		if executable != "" && (!filepath.IsAbs(executable) || strings.ContainsAny(executable, "\x00\r\n")) {
			return nil, errors.New("installation manifest executable paths must be absolute")
		}
	}
	return &manifest, nil
}

func managedCodexEntry(path, self string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if current, err := os.Stat(self); err == nil && os.SameFile(info, current) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil && filepath.Base(resolved) == "codex-gateway" {
		return true
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	prefix := make([]byte, 4096)
	n, _ := file.Read(prefix)
	return bytes.HasPrefix(prefix[:n], []byte("#!")) && bytes.Contains(prefix[:n], []byte("codex-gateway managed wrapper"))
}

func executableCandidate(path, self string) (string, bool) {
	if path == "" {
		return "", false
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || managedCodexEntry(resolved, self) {
		return "", false
	}
	absolute, err := filepath.Abs(resolved)
	return absolute, err == nil
}

func codexPathCandidates() []string {
	var paths []string
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory == "" || !filepath.IsAbs(directory) {
			continue // Never discover an executable in an untrusted current directory.
		}
		paths = append(paths, filepath.Join(directory, "codex"))
	}
	return paths
}

// Client launches preserve the original entry, including a user's backup or
// environment wrapper. Internal RPC helpers use a separate native resolver.
func codexExecutablePath(explicit string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", errors.New("cannot identify the gateway executable")
	}
	if explicit == "" {
		explicit = os.Getenv("CODEX_GATEWAY_CODEX_BIN")
	}
	if explicit != "" {
		if path, ok := executableCandidate(explicit, self); ok {
			return path, nil
		}
		return "", errors.New("selected Codex executable is missing or points back to codex-gateway")
	}
	manifest, err := readInstallManifest(filepath.Dir(self))
	if err != nil {
		return "", err
	}
	if manifest != nil {
		if path, ok := executableCandidate(manifest.OriginalExecutable, self); ok {
			return path, nil
		}
	}
	for _, candidate := range codexPathCandidates() {
		if path, ok := executableCandidate(candidate, self); ok {
			return path, nil
		}
	}
	return "", errors.New("cannot find the original Codex CLI; install Codex or set CODEX_GATEWAY_CODEX_BIN to its original executable")
}

func nativeBinary(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	var magic [4]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return false
	}
	return magic == [4]byte{0x7f, 'E', 'L', 'F'} || magic == [4]byte{0xcf, 0xfa, 0xed, 0xfe} ||
		magic == [4]byte{0xfe, 0xed, 0xfa, 0xcf} || magic == [4]byte{0xca, 0xfe, 0xba, 0xbe} || magic == [4]byte{0xca, 0xfe, 0xba, 0xbf}
}

// The official npm entry is a JavaScript launcher. Resolve its published native
// package layout without executing arbitrary user wrappers or modifying npm.
func npmNativeExecutable(path, self string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Base(resolved) != "codex.js" {
		return ""
	}
	root := filepath.Dir(filepath.Dir(resolved))
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	var pkg struct {
		Name string `json:"name"`
	}
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &pkg) != nil || pkg.Name != "@openai/codex" {
		return ""
	}
	arch, triple := "", ""
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		arch, triple = "linux-x64", "x86_64-unknown-linux-musl"
	case "linux/arm64":
		arch, triple = "linux-arm64", "aarch64-unknown-linux-musl"
	case "darwin/amd64":
		arch, triple = "darwin-x64", "x86_64-apple-darwin"
	case "darwin/arm64":
		arch, triple = "darwin-arm64", "aarch64-apple-darwin"
	default:
		return ""
	}
	roots := []string{filepath.Join(root, "node_modules", "@openai", "codex-"+arch), filepath.Join(filepath.Dir(root), "codex-"+arch), root}
	for _, packageRoot := range roots {
		for _, subdir := range []string{"bin", "codex"} {
			candidate := filepath.Join(packageRoot, "vendor", triple, subdir, "codex")
			if valid, ok := executableCandidate(candidate, self); ok && nativeBinary(valid) {
				return valid
			}
		}
	}
	return ""
}

func nativeCodexExecutablePath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", errors.New("cannot identify the gateway executable")
	}
	for _, key := range []string{"CODEX_GATEWAY_NATIVE_CODEX_BIN", "CODEX_GATEWAY_CODEX_BIN"} {
		if explicit := os.Getenv(key); explicit != "" {
			if path, ok := executableCandidate(explicit, self); ok {
				return path, nil // Explicit selection can intentionally be a trusted launcher.
			}
			return "", fmt.Errorf("%s must select the original Codex executable, not codex-gateway", key)
		}
	}
	manifest, err := readInstallManifest(filepath.Dir(self))
	if err != nil {
		return "", err
	}
	candidates := []string{}
	if manifest != nil {
		if path, ok := executableCandidate(manifest.NativeExecutable, self); ok {
			return path, nil
		}
		candidates = append(candidates, manifest.OriginalExecutable)
	}
	candidates = append(candidates, codexPathCandidates()...)
	for _, candidate := range candidates {
		path, ok := executableCandidate(candidate, self)
		if !ok {
			continue
		}
		if nativeBinary(path) {
			return path, nil
		}
		if native := npmNativeExecutable(path, self); native != "" {
			return native, nil
		}
	}
	return "", errors.New("cannot locate native Codex behind the installed launcher; set CODEX_GATEWAY_NATIVE_CODEX_BIN to the native executable")
}

func replaceEnvironment(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func wrappedInvocationSignature(args []string) string {
	_, remaining, err := lifecycleConfigArguments(args)
	if err != nil {
		remaining = args
	}
	data, _ := json.Marshal(struct {
		Args  []string
		Model string
	}{remaining, selectedWrappedModel(args)})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

var nativeValueOptions = map[string]bool{
	"-c": true, "--config": true, "-m": true, "--model": true, "-p": true, "--profile": true,
	"-C": true, "--cd": true, "-i": true, "--image": true, "-s": true, "--sandbox": true,
	"-a": true, "--ask-for-approval": true, "--enable": true, "--disable": true, "--add-dir": true,
	"--remote": true, "--remote-auth-token-env": true, "--local-provider": true,
	"-o": true, "--output-last-message": true, "--output-schema": true, "--thread-source": true,
}

func nativePositionals(args []string, start int) []int {
	var result []int
	for i := start; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			for i++; i < len(args); i++ {
				result = append(result, i)
			}
			break
		}
		if nativeValueOptions[arg] {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			continue
		}
		result = append(result, i)
	}
	return result
}

func wrappedCommand(args []string) (string, int) {
	positions := nativePositionals(args, 0)
	if len(positions) == 0 {
		return "", -1
	}
	for _, arg := range args[:positions[0]] {
		if arg == "--" {
			return "", -1
		}
	}
	return args[positions[0]], positions[0]
}

func nativePassthrough(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--help" || arg == "-h" || arg == "--version" || arg == "-V" || arg == "--oss" || strings.HasPrefix(arg, "--remote=") || arg == "--remote" {
			return true
		}
	}
	command, _ := wrappedCommand(args)
	switch command {
	case "help", "login", "logout", "mcp", "plugin", "app-server", "remote-control", "completion", "update", "doctor", "sandbox", "debug", "apply", "a", "queue", "archive", "delete", "migrate-rollouts", "unarchive", "cloud", "exec-server", "features", "agents":
		return true
	}
	return false
}

func nativeLoginAttempt(args []string) bool {
	command, index := wrappedCommand(args)
	if command != "login" || nativeHasFlag(args, "--help") || nativeHasFlag(args, "-h") || nativeHasFlag(args, "--version") || nativeHasFlag(args, "-V") {
		return false
	}
	positions := nativePositionals(args, index+1)
	return len(positions) == 0 || args[positions[0]] != "status"
}

func hasThirdPartyModels(cfg *Config) bool {
	for _, model := range cfg.Models {
		if provider, ok := cfg.Providers[model.Provider]; ok && provider.Auth == "api_key" {
			return true
		}
	}
	return false
}

func selectedWrappedModel(args []string) string {
	model := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if (arg == "-m" || arg == "--model") && i+1 < len(args) {
			i++
			model = args[i]
		} else if strings.HasPrefix(arg, "--model=") {
			model = strings.TrimPrefix(arg, "--model=")
		} else if strings.HasPrefix(arg, "-m") && !strings.HasPrefix(arg, "--") {
			model = strings.TrimPrefix(strings.TrimPrefix(arg, "-m"), "=")
		}
	}
	if model == "" {
		if overrides, _, err := lifecycleConfigArguments(args); err == nil {
			for _, override := range overrides {
				key, value, _ := strings.Cut(override, "=")
				if strings.TrimSpace(key) == "model" {
					var parsed struct {
						Model string `toml:"model"`
					}
					if toml.Unmarshal([]byte("model="+value), &parsed) == nil {
						model = parsed.Model
					} else {
						model = value // Native Codex treats a non-TOML value as a string.
					}
				}
			}
		}
	}
	return model
}

func remapWrappedModelArgs(args []string, remap func(string) string) []string {
	result := append([]string(nil), args...)
	rewriteOverride := func(override string) string {
		key, value, found := strings.Cut(override, "=")
		if !found || strings.TrimSpace(key) != "model" {
			return override
		}
		var parsed struct {
			Model string `toml:"model"`
		}
		if toml.Unmarshal([]byte("model="+value), &parsed) != nil {
			parsed.Model = value
		}
		if mapped := remap(parsed.Model); mapped != parsed.Model {
			return "model=" + lifecycleJSONString(mapped)
		}
		return override
	}
	for i := 0; i < len(result); i++ {
		arg := result[i]
		if arg == "--" {
			break
		}
		if (arg == "-c" || arg == "--config") && i+1 < len(result) {
			i++
			result[i] = rewriteOverride(result[i])
			continue
		}
		if strings.HasPrefix(arg, "--config=") {
			result[i] = "--config=" + rewriteOverride(strings.TrimPrefix(arg, "--config="))
			continue
		}
		if strings.HasPrefix(arg, "-c") && !strings.HasPrefix(arg, "--") {
			result[i] = "-c" + rewriteOverride(strings.TrimPrefix(strings.TrimPrefix(arg, "-c"), "="))
			continue
		}
		value, kind := "", 0
		if (arg == "-m" || arg == "--model") && i+1 < len(result) {
			i++
			value, kind = result[i], 1
		} else if strings.HasPrefix(arg, "--model=") {
			value, kind = strings.TrimPrefix(arg, "--model="), 2
		} else if strings.HasPrefix(arg, "-m") && !strings.HasPrefix(arg, "--") {
			value, kind = strings.TrimPrefix(strings.TrimPrefix(arg, "-m"), "="), 3
		}
		if mapped := remap(value); mapped != value {
			switch kind {
			case 1:
				result[i] = mapped
			case 2:
				result[i] = "--model=" + mapped
			case 3:
				result[i] = "-m" + mapped
			}
		}
	}
	return result
}

func officialModelArgs(args []string, cfg *Config) []string {
	return remapWrappedModelArgs(args, func(value string) string {
		if route, ok := cfg.Models[value]; ok && cfg.Providers[route.Provider].Auth == "codex" {
			return route.Model
		}
		return value
	})
}

func resolveWrappedModel(cfg *Config, model string) (string, error) {
	if _, ok := cfg.Models[model]; ok {
		return model, nil
	}
	var aliases []string
	for alias, route := range cfg.Models {
		if route.Model == model {
			aliases = append(aliases, alias)
		}
	}
	if len(aliases) == 0 {
		return model, nil // Unregistered models remain native Codex's responsibility.
	}
	for _, alias := range aliases {
		if alias == cfg.DefaultModel {
			return alias, nil
		}
	}
	if len(aliases) == 1 {
		return aliases[0], nil
	}
	sort.Strings(aliases)
	return "", fmt.Errorf("model %q is configured for multiple providers; select an alias with -m: %s", model, strings.Join(aliases, ", "))
}

func loadWrappedConfig(home string) (*Config, error) {
	if _, err := os.Stat(filepath.Join(home, "config.json")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return LoadConfig(home)
}

// ExecuteWrapped is the installed codex entry. Native utility commands keep
// their original arguments and launcher; only configured model sessions need
// gateway startup and process-local provider settings.
func ExecuteWrapped(args []string, out, errOut io.Writer) int {
	return executeWrapped(args, os.Stdin, out, errOut)
}

func executeWrapped(args []string, in io.Reader, out, errOut io.Writer) int {
	if previous := os.Getenv("CODEX_GATEWAY_WRAPPER_INVOCATION"); previous != "" && previous == wrappedInvocationSignature(args) {
		fmt.Fprintln(errOut, "error: the original Codex launcher called the gateway wrapper again; configure its native executable path")
		return 1
	}
	executable, err := codexExecutablePath("")
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return 1
	}
	home, err := ResolvePath(DefaultHome())
	if err != nil {
		fmt.Fprintln(errOut, "error: cannot resolve gateway home")
		return 1
	}
	cfg, configErr := loadWrappedConfig(home)
	utility := nativePassthrough(args)
	loginAttempt := nativeLoginAttempt(args)
	plain := utility
	if configErr != nil && !plain {
		fmt.Fprintln(errOut, "error:", configErr)
		return 1
	}
	if cfg == nil || !hasThirdPartyModels(cfg) {
		plain = true
	}
	if cfg != nil && !plain {
		if err := PrepareWrappedConfig(home, cfg); err != nil {
			fmt.Fprintln(errOut, "error:", err)
			return 1
		}
	}
	if cfg != nil && !utility {
		if model := selectedWrappedModel(args); model != "" {
			alias, err := resolveWrappedModel(cfg, model)
			if err != nil {
				fmt.Fprintln(errOut, "error:", err)
				return 1
			}
			if alias != model {
				args = remapWrappedModelArgs(args, func(value string) string {
					if value == model {
						return alias
					}
					return value
				})
			}
		}
	}
	var session *wrappedSession
	if cfg != nil && !plain {
		args, session, err = prepareWrappedResume(args, cfg, in, errOut, wrapperHistoryRPC)
		if errors.Is(err, errResumeCanceled) {
			return 0
		}
		if err != nil {
			fmt.Fprintln(errOut, "error:", err)
			return 1
		}
		if session != nil {
			plain = !sessionUsesGateway(*session, cfg)
			if selectedWrappedModel(args) == "" {
				if route, exists := cfg.Models[session.Model]; exists {
					model := session.Model
					if cfg.Providers[route.Provider].Auth == "codex" {
						model = route.Model
					}
					args = append([]string{"-m", model}, args...)
				}
			}
		}
	}
	if cfg != nil && !utility && hasThirdPartyModels(cfg) {
		if model := selectedWrappedModel(args); model != "" {
			route, ok := cfg.Models[model]
			plain = !ok || cfg.Providers[route.Provider].Auth == "codex"
		}
	}
	if !plain {
		code, err := RunCodex(home, args, executable)
		if err != nil {
			fmt.Fprintln(errOut, "error:", err)
		}
		return code
	}
	env := os.Environ()
	if cfg != nil {
		if !utility || loginAttempt {
			if err := ensureNativeHome(cfg); err != nil {
				fmt.Fprintln(errOut, "error:", err)
				return 1
			}
		}
		env = replaceEnvironment(env, "CODEX_HOME", cfg.CodexHome)
		if !utility {
			args = officialModelArgs(args, cfg)
			if resumeIndex, _ := wrappedResumeIndex(args); resumeIndex < 0 && selectedWrappedModel(args) == "" {
				if route, ok := cfg.Models[cfg.DefaultModel]; ok && cfg.Providers[route.Provider].Auth == "codex" {
					args = append([]string{"-m", route.Model}, args...)
				}
			}
		}
	}
	code, err := runCodexChild(executable, args, env, in, out, errOut)
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
	}
	if code == 0 && err == nil && loginAttempt && cfg != nil {
		if syncErr := RefreshOfficialAfterLogin(home, cfg); syncErr != nil {
			fmt.Fprintln(errOut, "warning: Codex login succeeded, but gateway model refresh failed:", syncErr)
		}
	}
	return code
}

type wrappedSession struct {
	ID            string `json:"id"`
	Model         string `json:"model"`
	ModelProvider string `json:"modelProvider"`
	Name          string `json:"name"`
	Preview       string `json:"preview"`
	Cwd           string `json:"cwd"`
	UpdatedAt     int64  `json:"updatedAt"`
}

type wrappedHistoryRPC func(context.Context, *Config, string, any, any) error

// A no-auth custom provider keeps local history queries independent of a
// ChatGPT subscription and never changes the user's stored login or provider.
func wrapperHistoryRPC(parent context.Context, cfg *Config, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	if err := ensureNativeHome(cfg); err != nil {
		return err
	}
	executable, err := nativeCodexExecutablePath()
	if err != nil {
		return err
	}
	settings := []string{`model_provider="codex-gateway-history"`, `model_providers.codex-gateway-history.name="Codex history reader"`,
		`model_providers.codex-gateway-history.base_url="http://127.0.0.1:9/v1"`, `model_providers.codex-gateway-history.env_key="CODEX_GATEWAY_HISTORY_TOKEN"`,
		`model_providers.codex-gateway-history.wire_api="responses"`, `model_providers.codex-gateway-history.requires_openai_auth=false`}
	args := []string{}
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	command := exec.CommandContext(ctx, executable, append(args, "app-server")...)
	command.Env = replaceEnvironment(replaceEnvironment(os.Environ(), "CODEX_HOME", cfg.CodexHome), "CODEX_GATEWAY_HISTORY_TOKEN", "local-history-only")
	command.Dir = cfg.CodexHome
	command.WaitDelay = time.Second
	return runNativeCodexRPC(ctx, command, method, params, result)
}

var errResumeCanceled = errors.New("resume selection canceled")

func nativeHasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == flag {
			return true
		}
	}
	return false
}

func wrappedResumeIndex(args []string) (int, bool) {
	command, index := wrappedCommand(args)
	if command == "resume" || command == "fork" {
		return index, false
	}
	if command == "exec" || command == "e" {
		positions := nativePositionals(args, index+1)
		if len(positions) > 0 && args[positions[0]] == "resume" {
			return positions[0], true
		}
	}
	return -1, false
}

func wrappedResumeCwd(args []string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if (arg == "-C" || arg == "--cd") && i+1 < len(args) {
			i++
			cwd = args[i]
		} else if strings.HasPrefix(arg, "--cd=") {
			cwd = strings.TrimPrefix(arg, "--cd=")
		} else if strings.HasPrefix(arg, "-C") && arg != "-C" {
			cwd = strings.TrimPrefix(strings.TrimPrefix(arg, "-C"), "=")
		}
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	return cwd, nil
}

func historyLabel(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > 90 {
		return string(runes[:90]) + "…"
	}
	return string(runes)
}

// Native resume --all only removes its directory filter, not its provider
// filter. This small selector asks native Codex for all providers and then
// hands an explicit ID back to the native resume/fork implementation.
func prepareWrappedResume(args []string, cfg *Config, in io.Reader, out io.Writer, rpc wrappedHistoryRPC) ([]string, *wrappedSession, error) {
	index, noninteractive := wrappedResumeIndex(args)
	if index < 0 {
		return args, nil, nil
	}
	positions := nativePositionals(args, index+1)
	last := nativeHasFlag(args[index+1:], "--last")
	if len(positions) > 0 && !last {
		var result struct {
			Thread wrappedSession `json:"thread"`
		}
		if err := rpc(context.Background(), cfg, "thread/read", map[string]any{"threadId": args[positions[0]], "includeTurns": false}, &result); err != nil {
			var listed struct {
				Data []wrappedSession `json:"data"`
			}
			if err := rpc(context.Background(), cfg, "thread/list", map[string]any{"modelProviders": []string{}, "searchTerm": args[positions[0]], "limit": 100, "sourceKinds": []string{"cli", "vscode", "exec", "appServer"}}, &listed); err == nil {
				var matches []wrappedSession
				for _, session := range listed.Data {
					if session.Name == args[positions[0]] {
						matches = append(matches, session)
					}
				}
				if len(matches) == 1 {
					return args, &matches[0], nil
				}
			}
			// Native Codex still owns name resolution and error handling for an
			// explicit session. A missing metadata match must not hide it.
			return args, &wrappedSession{ID: args[positions[0]]}, nil
		}
		return args, &result.Thread, nil
	}
	if noninteractive && !last {
		return args, nil, nil // Preserve native validation; never consume an exec prompt.
	}
	params := map[string]any{"modelProviders": []string{}, "limit": 20, "sortKey": "updated_at", "sortDirection": "desc"}
	if !nativeHasFlag(args[index+1:], "--all") {
		cwd, err := wrappedResumeCwd(args)
		if err != nil {
			return nil, nil, errors.New("cannot determine the resume directory")
		}
		params["cwd"] = cwd
	}
	if noninteractive || nativeHasFlag(args[index+1:], "--include-non-interactive") {
		params["sourceKinds"] = []string{"cli", "vscode", "exec", "appServer"}
	}
	for page := 0; page < 100; page++ {
		var result struct {
			Data       []wrappedSession `json:"data"`
			NextCursor string           `json:"nextCursor"`
		}
		if err := rpc(context.Background(), cfg, "thread/list", params, &result); err != nil {
			return nil, nil, fmt.Errorf("cannot list saved Codex sessions; retry or use an explicit session ID: %w", err)
		}
		if len(result.Data) == 0 {
			return nil, nil, errors.New("no saved Codex sessions found; use --all to include other directories")
		}
		choice := 0
		if !last {
			action := "Resume"
			if args[index] == "fork" {
				action = "Fork"
			}
			fmt.Fprintf(out, "%s a saved Codex session (all providers):\n", action)
			for i, session := range result.Data {
				label := session.Name
				if label == "" {
					label = session.Preview
				}
				fmt.Fprintf(out, "  %d. %s  [%s] %s", i+1, time.Unix(session.UpdatedAt, 0).Local().Format("2006-01-02 15:04"), historyLabel(session.ModelProvider), historyLabel(label))
				if nativeHasFlag(args[index+1:], "--all") {
					fmt.Fprintf(out, "  %s", historyLabel(session.Cwd))
				}
				fmt.Fprintln(out)
			}
			fmt.Fprint(out, "Session number (q to cancel")
			if result.NextCursor != "" {
				fmt.Fprint(out, ", n for more")
			}
			fmt.Fprint(out, "): ")
			line, err := readSetupLine(in)
			if errors.Is(err, io.EOF) || strings.EqualFold(strings.TrimSpace(line), "q") {
				return nil, nil, errResumeCanceled
			}
			if err != nil {
				return nil, nil, err
			}
			if strings.EqualFold(strings.TrimSpace(line), "n") && result.NextCursor != "" {
				params["cursor"] = result.NextCursor
				continue
			}
			number, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || number < 1 || number > len(result.Data) {
				return nil, nil, errors.New("select a session number from the list")
			}
			choice = number - 1
		}
		selected := result.Data[choice]
		if selected.ID == "" {
			return nil, nil, errors.New("native Codex returned a session without an ID")
		}
		updated := append([]string(nil), args[:index+1]...)
		updated = append(updated, selected.ID)
		for _, arg := range args[index+1:] {
			if arg != "--last" {
				updated = append(updated, arg)
			}
		}
		return updated, &selected, nil
	}
	return nil, nil, errors.New("too many saved session pages; resume using an explicit session ID")
}

func sessionUsesGateway(session wrappedSession, cfg *Config) bool {
	if route, ok := cfg.Models[session.Model]; ok {
		return cfg.Providers[route.Provider].Auth == "api_key"
	}
	return session.ModelProvider == "codex-gateway"
}
