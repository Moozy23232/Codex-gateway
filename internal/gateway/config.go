package gateway

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2"
)

func ResolvePath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	return filepath.Abs(path)
}

func DefaultHome() string {
	if value := os.Getenv("CODEX_GATEWAY_HOME"); value != "" {
		return value
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "codex-gateway")
}

// Reject duplicates before decoding into structs so configuration keys are unambiguous.
func checkJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var visit func(int) error
	visit = func(depth int) error {
		if depth > 128 {
			return errors.New("JSON nesting is too deep")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON key")
				}
				seen[name] = true
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

func readJSON(path string, value any, strict bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s; initialize the gateway or check the file", path)
	}
	if err := checkJSON(data); err != nil {
		return fmt.Errorf("invalid JSON in %s", path)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if strict {
		d.DisallowUnknownFields()
	}
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("invalid or unsupported fields in %s", path)
	}
	return nil
}

func PrivateWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gateway-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func WriteJSON(path string, value any) error {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetIndent("", "  ")
	e.SetEscapeHTML(false)
	if err := e.Encode(value); err != nil {
		return err
	}
	return PrivateWrite(path, b.Bytes())
}

func hasSpace(value string) bool { return strings.IndexFunc(value, unicode.IsSpace) >= 0 }

func validateURL(value, label string, allowHTTP, proxy bool) error {
	u, err := url.Parse(value)
	if err != nil || value == "" || hasSpace(value) || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("%s requires HTTP(S) without credentials, whitespace, query or fragment", label)
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid %s port", label)
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("invalid %s port", label)
	}
	if proxy && u.Path != "" && u.Path != "/" {
		return fmt.Errorf("%s cannot include a path", label)
	}
	ip := net.ParseIP(u.Hostname())
	if !proxy && !allowHTTP && u.Scheme == "http" && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s uses remote HTTP; explicitly allow insecure HTTP for a trusted network", label)
	}
	return nil
}

var providerName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
var modelAlias = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./:-]{0,255}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ValidateConfig(cfg *Config) error {
	if cfg == nil || cfg.Version != 1 {
		return errors.New("unsupported config version; expected version 1")
	}
	if cfg.Listen.Host != "127.0.0.1" || cfg.Listen.Port < 1 || cfg.Listen.Port > 65535 {
		return errors.New("listen must use 127.0.0.1 and a port between 1 and 65535")
	}
	if !filepath.IsAbs(cfg.CodexHome) {
		return errors.New("codex_home must be an absolute path")
	}
	if cfg.ClientAuth != "codex" && cfg.ClientAuth != "token" {
		return errors.New("client_auth must be codex or token")
	}
	if cfg.BootstrapProxy != "" {
		if err := validateURL(cfg.BootstrapProxy, "bootstrap_proxy", true, true); err != nil {
			return err
		}
	}
	if cfg.Providers == nil || cfg.Models == nil {
		return errors.New("providers and models must be objects")
	}
	for name, p := range cfg.Providers {
		if !providerName.MatchString(name) {
			return errors.New("invalid provider name")
		}
		if err := validateURL(p.BaseURL, "provider base_url", p.AllowInsecureHTTP, false); err != nil {
			return err
		}
		if p.Proxy != "" {
			if err := validateURL(p.Proxy, "provider proxy", true, true); err != nil {
				return err
			}
		}
		switch p.Auth {
		case "codex":
			if cfg.ClientAuth != "codex" || p.APIKeyEnv != "" || p.APIKeyFile != "" {
				return errors.New("codex providers require codex client auth and no API key reference")
			}
		case "api_key":
			if (p.APIKeyEnv == "") == (p.APIKeyFile == "") {
				return errors.New("specify exactly one api_key_env or api_key_file")
			}
			if p.APIKeyEnv != "" && !envName.MatchString(p.APIKeyEnv) {
				return errors.New("invalid API key environment variable name")
			}
			if p.APIKeyFile != "" && !filepath.IsAbs(p.APIKeyFile) {
				return errors.New("api_key_file must be an absolute path")
			}
		default:
			return errors.New("provider auth must be codex or api_key")
		}
	}
	for alias, m := range cfg.Models {
		if !modelAlias.MatchString(alias) {
			return errors.New("invalid model alias")
		}
		if _, ok := cfg.Providers[m.Provider]; !ok {
			return fmt.Errorf("model %s references an unknown provider", alias)
		}
		for _, v := range []string{m.Model, m.Template} {
			if v == "" || len(v) > 512 || hasSpace(v) {
				return fmt.Errorf("model %s requires nonempty model and template identifiers", alias)
			}
		}
	}
	if cfg.DefaultModel != "" {
		if _, ok := cfg.Models[cfg.DefaultModel]; !ok {
			return errors.New("default_model must name a configured model alias")
		}
	}
	return nil
}

func LoadConfig(home string) (*Config, error) {
	var cfg Config
	if err := readJSON(filepath.Join(home, "config.json"), &cfg, true); err != nil {
		return nil, err
	}
	if err := ValidateConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func ReadToken(home, name string) (string, error) {
	if name != "admin" && name != "client" {
		return "", errors.New("unknown local token type")
	}
	b, err := os.ReadFile(filepath.Join(home, name+"-token"))
	if err != nil {
		return "", fmt.Errorf("missing local %s token; initialize a gateway home", name)
	}
	token := strings.TrimSpace(string(b))
	if len(token) < 24 || hasSpace(token) {
		return "", fmt.Errorf("invalid local %s token", name)
	}
	return token, nil
}

func ResolveAPIKey(p Provider) (string, error) {
	var value string
	if p.APIKeyEnv != "" {
		value = os.Getenv(p.APIKeyEnv)
	} else {
		data, err := os.ReadFile(p.APIKeyFile)
		if err != nil {
			return "", errors.New("cannot read configured API key file")
		}
		value = string(data)
	}
	value = strings.TrimSpace(value)
	if value == "" || hasSpace(value) {
		return "", errors.New("configured API key is missing or invalid; check its environment variable or private file")
	}
	return value, nil
}

func TemplatesFrom(path string) (Catalog, error) {
	var catalog Catalog
	if err := readJSON(path, &catalog, false); err != nil {
		return catalog, err
	}
	if catalog.Models == nil {
		return catalog, errors.New("catalog must contain a models array")
	}
	seen := map[string]bool{}
	for _, item := range catalog.Models {
		slug, ok := item["slug"].(string)
		if !ok || slug == "" || seen[slug] {
			return catalog, errors.New("catalog requires unique nonempty slug fields")
		}
		seen[slug] = true
	}
	return catalog, nil
}

func LoadCatalog(home string) (Catalog, error) {
	return TemplatesFrom(filepath.Join(home, "models.json"))
}

func discoverTemplates(codexHome, path string) (Catalog, error) {
	if path != "" {
		return TemplatesFrom(path)
	}
	cache := filepath.Join(codexHome, "models_cache.json")
	if _, err := os.Stat(cache); err == nil {
		return TemplatesFrom(cache)
	}
	if data, err := os.ReadFile(filepath.Join(codexHome, "config.toml")); err == nil {
		var cfg struct {
			Catalog string `toml:"model_catalog_json"`
		}
		if toml.Unmarshal(data, &cfg) == nil && cfg.Catalog != "" {
			if _, err := os.Stat(cfg.Catalog); err == nil {
				return TemplatesFrom(cfg.Catalog)
			}
		}
	}
	return Catalog{Models: []map[string]any{}}, nil
}

func CatalogFor(cfg *Config, templates Catalog) (Catalog, error) {
	lookup := map[string]map[string]any{}
	for _, item := range templates.Models {
		slug, _ := item["slug"].(string)
		lookup[slug] = item
	}
	catalog := Catalog{Models: []map[string]any{}}
	aliases := make([]string, 0, len(cfg.Models))
	for alias := range cfg.Models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		route := cfg.Models[alias]
		source, ok := lookup[route.Template]
		if !ok {
			return catalog, fmt.Errorf("unknown template %s; import a Codex model catalog first", route.Template)
		}
		item := make(map[string]any, len(source))
		for key, value := range source {
			item[key] = value
		}
		item["slug"] = alias
		name := route.DisplayName
		if name == "" {
			base, _ := source["display_name"].(string)
			if base == "" {
				base = route.Template
			}
			name = base + " · " + route.Provider
		}
		item["display_name"], item["visibility"], item["supported_in_api"] = name, "list", true
		item["upgrade"], item["availability_nux"] = nil, nil
		item["additional_speed_tiers"], item["service_tiers"], item["use_responses_lite"] = []any{}, []any{}, false
		catalog.Models = append(catalog.Models, item)
	}
	return catalog, nil
}

func SaveConfig(home string, cfg *Config) error {
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	templates, err := TemplatesFrom(filepath.Join(home, "templates.json"))
	if err != nil {
		return err
	}
	catalog, err := CatalogFor(cfg, templates)
	if err != nil {
		return err
	}
	if err := WriteJSON(filepath.Join(home, "config.json"), cfg); err != nil {
		return err
	}
	return WriteJSON(filepath.Join(home, "models.json"), catalog)
}

func Fingerprint(home string) (string, error) {
	h := sha256.New()
	h.Write([]byte("go:" + Version))
	for _, name := range []string{"config.json", "models.json", "admin-token", "client-token"} {
		data, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			return "", fmt.Errorf("missing gateway file: %s", name)
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type InitOptions struct {
	CodexHome, AuthMode, CatalogFile, BootstrapProxy string
	Port                                             int
}

func Initialize(home string, options InitOptions) (*Config, error) {
	var err error
	home, err = ResolvePath(home)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(home)
	if err == nil && len(entries) > 0 {
		return nil, errors.New("gateway home is not empty; refusing to overwrite it")
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if options.CodexHome == "" {
		options.CodexHome = os.Getenv("CODEX_HOME")
		if options.CodexHome == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			options.CodexHome = filepath.Join(userHome, ".codex")
		}
	}
	options.CodexHome, err = ResolvePath(options.CodexHome)
	if err != nil {
		return nil, err
	}
	if options.AuthMode == "" {
		options.AuthMode = "codex"
	}
	if options.Port == 0 {
		options.Port = 33989
	}
	cfg := &Config{Version: 1, Listen: ListenConfig{Host: "127.0.0.1", Port: options.Port}, CodexHome: options.CodexHome, ClientAuth: options.AuthMode, BootstrapProxy: options.BootstrapProxy, Providers: map[string]Provider{}, Models: map[string]Model{}}
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	templates, err := discoverTemplates(options.CodexHome, options.CatalogFile)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(home, 0700); err != nil {
		return nil, err
	}
	if err := WriteJSON(filepath.Join(home, "templates.json"), templates); err != nil {
		return nil, err
	}
	for _, name := range []string{"admin", "client"} {
		data := make([]byte, 32)
		if _, err := rand.Read(data); err != nil {
			return nil, err
		}
		if err := PrivateWrite(filepath.Join(home, name+"-token"), []byte(base64.RawURLEncoding.EncodeToString(data)+"\n")); err != nil {
			return nil, err
		}
	}
	if err := SaveConfig(home, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
