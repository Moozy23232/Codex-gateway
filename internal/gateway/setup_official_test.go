package gateway

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareWrappedConfigPreservesAPIUseWhenOfficialRefreshFails(t *testing.T) {
	home, cfg := configFixture(t)
	defaultModel := cfg.DefaultModel
	if err := os.MkdirAll(cfg.CodexHome, 0700); err != nil {
		t.Fatal(err)
	}
	officialTestCredential(t, cfg, "setup-fake-native-token")
	beforeAuth, err := os.ReadFile(filepath.Join(cfg.CodexHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_GATEWAY_NATIVE_CODEX_BIN", filepath.Join(t.TempDir(), "missing-native"))
	if err := PrepareWrappedConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadConfig(home)
	if err != nil || saved.DefaultModel != defaultModel || saved.Models[defaultModel] != cfg.Models[defaultModel] {
		t.Fatalf("offline official refresh changed the active API route: %v", err)
	}
	if !saved.Providers["official"].AutoModels || saved.Providers["official"].Auth != "codex" {
		t.Fatal("existing native subscription was not recognized automatically")
	}
	afterAuth, err := os.ReadFile(filepath.Join(cfg.CodexHome, "auth.json"))
	if err != nil || !bytes.Equal(beforeAuth, afterAuth) {
		t.Fatal("wrapper preparation changed native credentials")
	}
	data, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if bytes.Contains(data, []byte("setup-fake-native-token")) {
		t.Fatal("native credential was copied into gateway config")
	}
}

func TestPrepareWrappedConfigDoesNothingWithoutNativeLogin(t *testing.T) {
	home, cfg := configFixture(t)
	before, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_GATEWAY_NATIVE_CODEX_BIN", filepath.Join(t.TempDir(), "must-not-run"))
	if err := PrepareWrappedConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("API-only startup changed configuration")
	}
	if _, err := os.Stat(cfg.CodexHome); !os.IsNotExist(err) {
		t.Fatal("API-only preparation started a native login or created native state")
	}
}
