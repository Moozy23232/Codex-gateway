package gateway

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFixture(t *testing.T) (string, *Config) {
	t.Helper()
	root := t.TempDir()
	catalog := filepath.Join(root, "catalog.json")
	if err := os.WriteFile(catalog, []byte(`{"models":[{"slug":"template","display_name":"Template","context_window":128000,"nested":{"large":9007199254740993}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "gateway")
	cfg, err := Initialize(home, InitOptions{AuthMode: "token", CodexHome: filepath.Join(root, "codex"), CatalogFile: catalog})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["example"] = Provider{BaseURL: "http://127.0.0.1:9999/v1", Auth: "api_key", APIKeyEnv: "GATEWAY_TEST_KEY"}
	cfg.Models["example/coding"] = Model{Provider: "example", Model: "upstream", Template: "template"}
	cfg.DefaultModel = "example/coding"
	if err := SaveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	return home, cfg
}

func TestConfigurationRoundTripAndPrivateFiles(t *testing.T) {
	home, cfg := configFixture(t)
	got, err := LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.Models["example/coding"] != cfg.Models["example/coding"] {
		t.Fatal("model mapping changed")
	}
	for _, name := range []string{"config.json", "templates.json", "models.json", "admin-token", "client-token"} {
		info, err := os.Stat(filepath.Join(home, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode = %o", name, info.Mode().Perm())
		}
	}
	admin, _ := ReadToken(home, "admin")
	client, _ := ReadToken(home, "client")
	if admin == client || len(admin) < 32 {
		t.Fatal("independent random tokens required")
	}
	if _, err := Initialize(home, InitOptions{}); err == nil {
		t.Fatal("init overwrote existing home")
	}
	if again, _ := ReadToken(home, "admin"); again != admin {
		t.Fatal("init changed existing token")
	}
	catalog, err := LoadCatalog(home)
	if err != nil {
		t.Fatal(err)
	}
	model := catalog.Models[0]
	if model["slug"] != "example/coding" || model["supported_in_api"] != true {
		t.Fatal("catalog mapping missing")
	}
	if model["nested"].(map[string]any)["large"].(json.Number).String() != "9007199254740993" {
		t.Fatal("catalog integer lost precision")
	}
}

func TestConfigRejectsInvalidChangesBeforeSaving(t *testing.T) {
	cases := map[string]func(*Config){
		"public listener": func(c *Config) { c.Listen.Host = "0.0.0.0" },
		"remote insecure HTTP": func(c *Config) {
			p := c.Providers["example"]
			p.BaseURL = "http://api.example.com/v1"
			c.Providers["example"] = p
		},
		"URL credentials": func(c *Config) {
			p := c.Providers["example"]
			p.BaseURL = "https://user:pass@example.com/v1"
			c.Providers["example"] = p
		},
		"query": func(c *Config) {
			p := c.Providers["example"]
			p.BaseURL = "https://api.example.com/v1?api-version=1"
			c.Providers["example"] = p
		},
		"two key refs": func(c *Config) { p := c.Providers["example"]; p.APIKeyFile = "/tmp/key"; c.Providers["example"] = p },
		"native credentials at nonofficial endpoint in token mode": func(c *Config) {
			p := c.Providers["example"]
			p.Auth = "codex"
			p.APIKeyEnv = ""
			c.Providers["example"] = p
		},
		"unknown template": func(c *Config) {
			m := c.Models["example/coding"]
			m.Template = "missing"
			c.Models["example/coding"] = m
		},
		"missing provider": func(c *Config) { delete(c.Providers, "example") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			home, cfg := configFixture(t)
			before, _ := os.ReadFile(filepath.Join(home, "config.json"))
			change(cfg)
			if err := SaveConfig(home, cfg); err == nil {
				t.Fatal("invalid configuration accepted")
			}
			after, _ := os.ReadFile(filepath.Join(home, "config.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("failed save changed config")
			}
		})
	}
}

func TestTokenModeAcceptsOnlyOfficialNativeCredentialEndpoint(t *testing.T) {
	home, cfg := configFixture(t)
	cfg.Providers["official"] = Provider{Auth: "codex", BaseURL: officialBaseURL}
	if err := SaveConfig(home, cfg); err != nil {
		t.Fatalf("mixed API key and official configuration: %v", err)
	}
	for _, endpoint := range []string{
		"https://chatgpt.com.example.com/backend-api/codex",
		"https://chatgpt.com/backend-api/other",
		"http://chatgpt.com/backend-api/codex",
		"https://chatgpt.com/backend-api/codex?redirect=1",
	} {
		p := cfg.Providers["official"]
		p.BaseURL = endpoint
		cfg.Providers["official"] = p
		if err := ValidateConfig(cfg); err == nil {
			t.Fatal("accepted a nonofficial native credential destination")
		}
	}
}

func TestConfigRejectsAmbiguousOrUnknownJSON(t *testing.T) {
	for _, mutate := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"version": 1`, `"version": 1, "version": 1`, 1) },
		func(s string) string {
			return strings.Replace(s, `"version": 1`, `"version": 1, "api_key": "do-not-store"`, 1)
		},
		func(s string) string { return s + `{}` },
		func(s string) string { return strings.Replace(s, `"base_url":`, `"unexpected": true, "base_url":`, 1) },
	} {
		home, _ := configFixture(t)
		path := filepath.Join(home, "config.json")
		data, _ := os.ReadFile(path)
		if err := os.WriteFile(path, []byte(mutate(string(data))), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(home); err == nil {
			t.Fatal("ambiguous/unknown config accepted")
		}
	}
}

func TestDiscoverTemplatesUsesTOMLParser(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "catalog.json")
	if err := WriteJSON(path, Catalog{Models: []map[string]any{{"slug": "example"}}}); err != nil {
		t.Fatal(err)
	}
	data := "model_catalog_json = '" + path + "' # comment\n[profiles.other]\nmodel_catalog_json = '/missing'\n"
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := discoverTemplates(root, "")
	if err != nil || len(catalog.Models) != 1 {
		t.Fatalf("catalog discovery: %v", err)
	}
}

func TestCLIConfigurationWorkflowAndKeyPrivacy(t *testing.T) {
	home, _ := configFixture(t)
	t.Setenv("GATEWAY_TEST_KEY", "test-value-never-print")
	call := func(want int, args ...string) string {
		t.Helper()
		var out, errs bytes.Buffer
		code := Execute(append([]string{"--home", home}, args...), &out, &errs)
		if code != want {
			t.Fatalf("%v exit %d, stderr %s", args, code, errs.String())
		}
		if strings.Contains(out.String()+errs.String(), "test-value-never-print") {
			t.Fatal("key was printed")
		}
		return out.String()
	}
	call(0, "validate", "--credentials")
	call(0, "provider", "add", "second", "--base-url", "https://api.example.com/v1", "--api-key-env", "GATEWAY_TEST_KEY")
	call(0, "model", "add", "second/coding", "--provider", "second", "--upstream-model", "model-x", "--template", "template", "--default")
	call(2, "provider", "remove", "second")
	call(0, "config", "show")
	call(0, "provider", "list")
	call(0, "model", "remove", "second/coding")
	call(0, "provider", "remove", "second")
	call(0, "config", "set", "default_model", "example/coding")
	call(2, "provider", "add", "example", "--base-url", "https://api.example.com", "--api-key-env", "GATEWAY_TEST_KEY")
	call(2, "model", "add", "bad", "--provider", "example", "--upstream-model", "x", "--template", "missing")
	call(0, "catalog", "build")
	data, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if bytes.Contains(data, []byte("test-value-never-print")) {
		t.Fatal("key was persisted")
	}
}
