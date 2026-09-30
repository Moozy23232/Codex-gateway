package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeTestModel(t *testing.T, id string) map[string]any {
	t.Helper()
	catalog, err := decodeNativeModels(bundledNativeModels)
	if err != nil {
		t.Fatal(err)
	}
	item := findModelTemplate(catalog, id)
	if item == nil {
		t.Fatalf("public test model %s is missing", id)
	}
	return item
}

func TestNativeCapabilitiesUseNewestCompleteCacheAndIgnoreAliasConfig(t *testing.T) {
	home, cfg := emptyModelSetupHome(t)
	old := nativeTestModel(t, "gpt-6.1-sol")
	old["context_window"] = 100000
	newer := nativeTestModel(t, "gpt-6.1-sol")
	newer["context_window"] = 200000
	nativePath := filepath.Join(cfg.CodexHome, "models_cache.json")
	gatewayPath := filepath.Join(home, "official-models-cache.json")
	for path, item := range map[string]map[string]any{nativePath: old, gatewayPath: newer} {
		if err := WriteJSON(path, Catalog{Models: []map[string]any{item}}); err != nil {
			t.Fatal(err)
		}
	}
	setTime := func(path string, seconds int64) {
		t.Helper()
		stamp := time.Unix(seconds, 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	setTime(nativePath, 100)
	setTime(gatewayPath, 200)
	check := func(want string) {
		t.Helper()
		catalog, err := nativeModelsForSetup(home, cfg)
		if err != nil || findModelTemplate(catalog, "gpt-6.1-sol")["context_window"] != json.Number(want) {
			t.Fatalf("latest complete capabilities were not selected: %v", err)
		}
	}
	check("200000")
	setTime(nativePath, 300)
	check("100000")
	old["supported_reasoning_levels"] = []any{}
	if err := WriteJSON(nativePath, Catalog{Models: []map[string]any{old}}); err != nil {
		t.Fatal(err)
	}
	check("200000") // A newer incomplete cache cannot replace full metadata.
	if err := os.Remove(nativePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gatewayPath); err != nil {
		t.Fatal(err)
	}
	aliasCatalog := filepath.Join(t.TempDir(), "aliases.json")
	if err := WriteJSON(aliasCatalog, Catalog{Models: []map[string]any{newer}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CodexHome, "config.toml"), []byte(fmt.Sprintf("model_catalog_json = %q\n", aliasCatalog)), 0600); err != nil {
		t.Fatal(err)
	}
	check("272000")
}

func TestNativeModelChoicesAndNoticesNeedNoAccount(t *testing.T) {
	home, cfg := emptyModelSetupHome(t)
	t.Setenv("CODEX_GATEWAY_NATIVE_CODEX_BIN", filepath.Join(t.TempDir(), "no-process"))
	choices, err := GetNativeModelChoices(home, cfg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, choice := range choices {
		found = found || choice.ID == "gpt-6.1-sol"
	}
	if !found {
		t.Fatal("public native GPT choices require an account")
	}
	if _, err := os.Stat(filepath.Join(cfg.CodexHome, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("offline capabilities created authentication state")
	}
	notices := NativeModelNotices()
	for _, expected := range []string{"rust-v0.159.2", "ff6aec96948b70d94983af2641a6b67c94faeff5", "Apache License"} {
		if !strings.Contains(notices, expected) {
			t.Fatalf("binary attribution is missing %s", expected)
		}
	}
}
