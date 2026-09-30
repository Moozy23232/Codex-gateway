package gateway

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Read a byte at a time: buffering stdin here would consume bytes intended for
// the subsequent hidden terminal prompt. A final line without LF is accepted.
func readSetupLine(in io.Reader) (string, error) {
	var line []byte
	var one [1]byte
	for {
		n, err := in.Read(one[:])
		if n > 0 {
			if one[0] == '\n' {
				return strings.TrimSpace(string(line)), nil
			}
			if one[0] == 3 || one[0] == 4 {
				return "", errors.New("input cancelled")
			}
			line = append(line, one[0])
			if len(line) > 64*1024 {
				return "", errors.New("input is too long")
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return strings.TrimSpace(string(line)), nil
			}
			return "", err
		}
		if n == 0 {
			return "", io.ErrNoProgress
		}
	}
}

func setupPrompt(in io.Reader, out io.Writer, label string) (string, error) {
	fmt.Fprint(out, label)
	value, err := readSetupLine(in)
	if err != nil {
		return "", errors.New("input ended or was cancelled; rerun the command or supply the missing options")
	}
	if value == "" {
		return "", errors.New("a nonempty value is required")
	}
	return value, nil
}

// A new home is planned in memory so help, invalid input and cancellation have
// no effect on disk. Existing homes keep their selected authentication mode.
func setupConfig(home string) (*Config, bool, error) {
	if _, err := os.Lstat(filepath.Join(home, "config.json")); err == nil {
		cfg, err := LoadConfig(home)
		return cfg, false, err
	} else if !os.IsNotExist(err) {
		return nil, false, err
	}
	entries, err := os.ReadDir(home)
	if err == nil && len(entries) != 0 {
		return nil, false, errors.New("gateway home is not empty and has no config.json; refusing to overwrite it")
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, false, err
	}
	cfg, err := initialConfig(InitOptions{AuthMode: "token"})
	return cfg, true, err
}

func addProvider(home string, args []string, in io.Reader, out io.Writer) error {
	f := newFlags("add-provider [NAME]", out)
	var p Provider
	official := f.Bool("official", false, "use the official Codex ChatGPT subscription and native login")
	f.StringVar(&p.BaseURL, "base-url", "", "Responses-compatible base URL (prompted if omitted)")
	f.StringVar(&p.APIKeyEnv, "api-key-env", "", "environment variable name, never the key")
	f.StringVar(&p.APIKeyFile, "api-key-file", "", "private key file; otherwise securely prompt for a key")
	f.StringVar(&p.Proxy, "proxy", "", "explicit upstream HTTP proxy; otherwise connect directly")
	f.BoolVar(&p.AllowInsecureHTTP, "allow-insecure-http", false, "allow remote HTTP on a trusted network")
	replace := f.Bool("replace", false, "replace an existing provider with these complete settings")
	positions, err := parseFlags(f, args)
	if err != nil {
		return err
	}
	if len(positions) > 1 {
		return errors.New("expected at most one provider name; see --help")
	}
	if *official && (p.BaseURL != "" || p.APIKeyEnv != "" || p.APIKeyFile != "" || p.AllowInsecureHTTP) {
		return errors.New("--official uses the official endpoint and native login; omit base URL and API key options")
	}
	if p.APIKeyEnv != "" && p.APIKeyFile != "" {
		return errors.New("specify only one of --api-key-env and --api-key-file")
	}
	cfg, fresh, err := setupConfig(home)
	if err != nil {
		return err
	}
	name := ""
	if len(positions) == 1 {
		name = positions[0]
	} else if *official {
		name = "official"
	} else {
		name, err = setupPrompt(in, out, "Provider name: ")
		if err != nil {
			return err
		}
	}
	if !providerName.MatchString(name) {
		return errors.New("provider name must start with a letter and contain up to 64 letters, digits, underscores or hyphens")
	}
	if _, exists := cfg.Providers[name]; exists && !*replace {
		return errors.New("provider already exists; use --replace")
	}
	var secret string
	if *official {
		p.Auth, p.BaseURL = "codex", officialBaseURL
	} else {
		p.Auth = "api_key"
		if p.BaseURL == "" {
			p.BaseURL, err = setupPrompt(in, out, "Responses base URL: ")
			if err != nil {
				return err
			}
		}
		if p.APIKeyFile != "" {
			p.APIKeyFile, err = ResolvePath(p.APIKeyFile)
			if err != nil {
				return err
			}
		}
	}
	promptKey := !*official && p.APIKeyEnv == "" && p.APIKeyFile == ""
	if promptKey {
		// Placeholder for validation only; the actual file gets a unique name.
		p.APIKeyFile = filepath.Join(home, "keys", "pending.key")
	}
	cfg.Providers[name] = p
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	if !fresh {
		templates, err := TemplatesFrom(filepath.Join(home, "templates.json"))
		if err != nil {
			return err
		}
		if _, err := CatalogFor(cfg, templates); err != nil {
			return err
		}
	}
	if promptKey {
		fmt.Fprint(out, "API key (hidden): ")
		secret, err = readSetupSecret(in, out)
		if err != nil {
			return errors.New("API key input ended or was cancelled")
		}
		secret = strings.TrimSpace(secret)
		if secret == "" || hasSpace(secret) || hasControl(secret) {
			return errors.New("API key is empty or invalid")
		}
	} else if !*official {
		if _, err := ResolveAPIKey(p); err != nil {
			return err
		}
	}
	if *official {
		if err := EnsureOfficialLogin(home, cfg); err != nil {
			return err
		}
	}
	if fresh {
		if _, err := Initialize(home, InitOptions{AuthMode: cfg.ClientAuth, CodexHome: cfg.CodexHome, Port: cfg.Listen.Port}); err != nil {
			return err
		}
	}
	keyPath := ""
	if promptKey {
		keyPath, err = saveSetupKey(home, name, secret)
		if err != nil {
			return err
		}
		p.APIKeyFile = keyPath
		cfg.Providers[name] = p
	}
	if err := SaveConfig(home, cfg); err != nil {
		// SaveConfig may have saved config.json before a later I/O error.
		// Never delete a key that the persisted configuration already references.
		if keyPath != "" {
			persisted, readErr := LoadConfig(home)
			if readErr == nil && persisted.Providers[name].APIKeyFile != keyPath {
				_ = os.Remove(keyPath)
			}
		}
		return err
	}
	fmt.Fprintf(out, "Added provider %s. Next: codex-gateway add-model --provider %s\n", name, name)
	return nil
}

func saveSetupKey(home, name, secret string) (string, error) {
	dir := filepath.Join(home, "keys")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, name+"-*.key")
	if err != nil {
		return "", err
	}
	path, saved := f.Name(), false
	defer func() {
		_ = f.Close()
		if !saved {
			_ = os.Remove(path)
		}
	}()
	if _, err := io.WriteString(f, secret+"\n"); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	saved = true
	return path, nil
}

func addModel(home string, args []string, in io.Reader, out io.Writer) error {
	f := newFlags("add-model [MODEL_ID]", out)
	var m Model
	var options ModelTemplateOptions
	f.StringVar(&m.Provider, "provider", "", "configured provider; automatically selected when only one exists")
	alias := f.String("alias", "", "Codex model alias (default: provider/model ID)")
	f.StringVar(&m.DisplayName, "display-name", "", "name shown in Codex")
	f.StringVar(&options.Template, "template", "", "optional compatible catalog template slug")
	f.IntVar(&options.ContextWindow, "context-window", 0, "known model context window; otherwise use template or conservative default")
	f.StringVar(&options.ReasoningEffort, "reasoning-effort", "", "supported reasoning effort; generic models do not enable reasoning by default")
	def := f.Bool("default", false, "make this the default model")
	replace := f.Bool("replace", false, "replace an existing model alias")
	positions, err := parseFlags(f, args)
	if err != nil {
		return err
	}
	if len(positions) > 1 {
		return errors.New("expected at most one model ID; see --help")
	}
	if options.ContextWindow < 0 {
		return errors.New("--context-window must be positive when specified")
	}
	cfg, fresh, err := setupConfig(home)
	if err != nil {
		return err
	}
	if fresh || len(cfg.Providers) == 0 {
		return errors.New("add a provider first: codex-gateway add-provider [NAME] [--official]")
	}
	if m.Provider == "" {
		names := make([]string, 0, len(cfg.Providers))
		for name := range cfg.Providers {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) == 1 {
			m.Provider = names[0]
		} else {
			for i, name := range names {
				fmt.Fprintf(out, "%d. %s\n", i+1, name)
			}
			m.Provider, err = setupPrompt(in, out, "Provider (number or name): ")
			if err != nil {
				return err
			}
			if n, err := strconv.Atoi(m.Provider); err == nil && n > 0 && n <= len(names) {
				m.Provider = names[n-1]
			}
		}
	}
	if _, ok := cfg.Providers[m.Provider]; !ok {
		return errors.New("unknown provider; use provider list to see configured names")
	}
	if len(positions) == 1 {
		m.Model = positions[0]
	} else {
		choices, discoverErr := DiscoverProviderModels(home, cfg, m.Provider)
		if discoverErr != nil {
			fmt.Fprintln(out, "Could not list this provider's models. Enter a model ID manually.")
		}
		for i, choice := range choices {
			fmt.Fprintf(out, "%d. %s\n", i+1, choice.ID)
		}
		m.Model, err = setupPrompt(in, out, "Model (number or model ID): ")
		if err != nil {
			return err
		}
		if n, err := strconv.Atoi(m.Model); err == nil && n > 0 && n <= len(choices) {
			m.Model = choices[n-1].ID
		}
	}
	if *alias == "" {
		*alias = setupModelAlias(m.Provider, m.Model)
	}
	if _, ok := cfg.Models[*alias]; ok && !*replace {
		return errors.New("model already exists; use --replace or choose another --alias")
	}
	// Validate the route before PrepareModelTemplate persists any new metadata.
	m.Template = "setup-pending"
	cfg.Models[*alias] = m
	if *def || cfg.DefaultModel == "" {
		cfg.DefaultModel = *alias
	}
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	m.Template, err = PrepareModelTemplate(home, cfg, m.Model, options)
	if err != nil {
		return err
	}
	if m.DisplayName == "" {
		m.DisplayName = m.Model + " · " + m.Provider
	}
	cfg.Models[*alias] = m
	if err := SaveConfig(home, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "Added model %s. Run codex-gateway to open Codex.\n", *alias)
	return nil
}

func setupModelAlias(provider, model string) string {
	alias := provider + "/" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./:-", r) {
			return r
		}
		return '_'
	}, model)
	if len(alias) > 256 {
		hash := sha256.Sum256([]byte(alias))
		alias = fmt.Sprintf("%s-%x", alias[:239], hash[:8])
	}
	return alias
}
