//go:build linux || darwin

package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const setupFakeKey = "setup-only-private-key-never-print"

type setupNoInput struct{ reads int }

func (r *setupNoInput) Read([]byte) (int, error) {
	r.reads++
	return 0, io.ErrUnexpectedEOF
}

func setupTestHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("GATEWAY_SETUP_TEST_KEY", setupFakeKey)
	// Any accidental native helper call fails locally instead of entering the
	// developer's active Codex wrapper or using real account credentials.
	t.Setenv("CODEX_GATEWAY_CODEX_BIN", filepath.Join(root, "no-native-codex"))
	return filepath.Join(root, "gateway")
}

func setupCall(t *testing.T, home string, in io.Reader, want int, args ...string) string {
	t.Helper()
	var out, errs bytes.Buffer
	code := ExecuteWithInput(append([]string{"--home", home}, args...), in, &out, &errs)
	if strings.Contains(out.String()+errs.String(), setupFakeKey) {
		t.Fatal("setup printed an API key")
	}
	if code != want {
		t.Fatalf("setup %v exited %d, want %d: %s", args, code, want, errs.String())
	}
	return out.String() + errs.String()
}

func setupSnapshot(t *testing.T, home string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if _, err := os.Stat(home); os.IsNotExist(err) {
		return files
	}
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(home, path)
		if err == nil {
			files[rel] = string(data)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func setupAddKeyProvider(t *testing.T, home, name string) {
	t.Helper()
	in := &setupNoInput{}
	setupCall(t, home, in, 0, "add-provider", name, "--base-url", "https://api.example.invalid/v1", "--api-key-env", "GATEWAY_SETUP_TEST_KEY")
	if in.reads != 0 {
		t.Fatal("complete provider options unexpectedly prompted")
	}
}

func TestSetupProviderAndModelWorkWithoutManualInitialization(t *testing.T) {
	home := setupTestHome(t)
	setupAddKeyProvider(t, home, "example")
	cfg, err := LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != "token" || cfg.Providers["example"].Auth != "api_key" {
		t.Fatal("new third-party provider depends on native account login")
	}
	if len(cfg.Models) != 0 {
		t.Fatal("provider setup invented model routes")
	}
	in := &setupNoInput{}
	setupCall(t, home, in, 0, "add-model", "model-one")
	if in.reads != 0 {
		t.Fatal("one provider plus an explicit model unexpectedly prompted")
	}
	cfg, err = LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	first := cfg.Models["example/model-one"]
	if first.Provider != "example" || first.Model != "model-one" || first.Template == "" || cfg.DefaultModel != "example/model-one" {
		t.Fatal("automatic route, metadata or first-model default missing")
	}
	catalog, err := LoadCatalog(home)
	if err != nil || len(catalog.Models) != 1 || catalog.Models[0]["slug"] != "example/model-one" {
		t.Fatalf("automatic catalog is not usable: %v", err)
	}
	setupCall(t, home, in, 0, "add-model", "model-two")
	cfg, _ = LoadConfig(home)
	if cfg.DefaultModel != "example/model-one" {
		t.Fatal("adding another model changed the user's default")
	}
	setupCall(t, home, in, 0, "add-model", "model-three", "--alias", "favorite", "--default")
	cfg, _ = LoadConfig(home)
	if cfg.DefaultModel != "favorite" || cfg.Models["favorite"].Model != "model-three" {
		t.Fatal("explicit alias/default was ignored")
	}
}

func TestSetupPromptedKeyIsPrivateAndReplacementKeepsTheOldKey(t *testing.T) {
	home := setupTestHome(t)
	input := "example\nhttps://api.example.invalid/v1\n" + setupFakeKey + "\n"
	setupCall(t, home, strings.NewReader(input), 0, "add-provider")
	cfg, err := LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := cfg.Providers["example"].APIKeyFile
	rel, err := filepath.Rel(home, keyPath)
	if err != nil || !filepath.IsAbs(keyPath) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatal("prompted key was stored outside its gateway home")
	}
	data, err := os.ReadFile(keyPath)
	if err != nil || strings.TrimSpace(string(data)) != setupFakeKey {
		t.Fatalf("managed key file was not created: %v", err)
	}
	for path, mode := range map[string]fs.FileMode{keyPath: 0600, filepath.Dir(keyPath): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("managed key permissions: %v", err)
		}
	}
	for name, contents := range setupSnapshot(t, home) {
		if name != rel && strings.Contains(contents, setupFakeKey) {
			t.Fatalf("key copied into %s", name)
		}
	}
	setupCall(t, home, strings.NewReader("replacement-test-key\n"), 0, "add-provider", "example", "--base-url", "https://replacement.example.invalid/v1", "--replace")
	after, err := LoadConfig(home)
	if err != nil || after.Providers["example"].APIKeyFile == keyPath {
		t.Fatalf("replacement reused the live provider's key file: %v", err)
	}
	old, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(old, data) {
		t.Fatal("replacement changed an older gateway process's key")
	}
}

func TestSetupExternalKeyFileDoesNotPromptOrCopyTheKey(t *testing.T) {
	home := setupTestHome(t)
	key := filepath.Join(t.TempDir(), "external.key")
	if err := os.WriteFile(key, []byte(setupFakeKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	in := &setupNoInput{}
	setupCall(t, home, in, 0, "add-provider", "example", "--base-url", "https://api.example.invalid/v1", "--api-key-file", key)
	if in.reads != 0 {
		t.Fatal("external file reference unexpectedly prompted")
	}
	cfg, _ := LoadConfig(home)
	if cfg.Providers["example"].APIKeyFile != key {
		t.Fatal("external key reference was replaced")
	}
	for name, contents := range setupSnapshot(t, home) {
		if strings.Contains(contents, setupFakeKey) {
			t.Fatalf("external key copied into %s", name)
		}
	}
}

func TestSetupOfficialUsesNativeLoginAndKeepsThirdPartyIndependent(t *testing.T) {
	home := setupTestHome(t)
	setupAddKeyProvider(t, home, "example")
	cfg, err := LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.CodexHome, 0700); err != nil {
		t.Fatal(err)
	}
	nativeConfig := filepath.Join(cfg.CodexHome, "config.toml")
	original := []byte("model = \"my-existing-choice\"\n")
	if err := os.WriteFile(nativeConfig, original, 0600); err != nil {
		t.Fatal(err)
	}
	officialTestExecutable(t)
	record := filepath.Join(t.TempDir(), "login.json")
	t.Setenv("CODEX_GATEWAY_OFFICIAL_RECORD", record)
	in := &setupNoInput{}
	setupCall(t, home, in, 0, "add-provider", "--official")
	if in.reads != 0 {
		t.Fatal("official provider asked the gateway for an API key")
	}
	cfg, err = LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != "token" || cfg.Providers["official"].Auth != "codex" || cfg.Providers["example"].APIKeyEnv != "GATEWAY_SETUP_TEST_KEY" {
		t.Fatal("official setup changed third-party client authentication")
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatal("official provider did not invoke the native login helper")
	}
	for name, contents := range setupSnapshot(t, home) {
		if strings.Contains(contents, "fake-login-token") {
			t.Fatalf("native account token copied into gateway file %s", name)
		}
	}
	after, err := os.ReadFile(nativeConfig)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("official setup rewrote native Codex configuration")
	}
}

func TestSetupInvalidAndCanceledOperationsPreserveConfiguration(t *testing.T) {
	home := setupTestHome(t)
	setupAddKeyProvider(t, home, "example")
	setupCall(t, home, &setupNoInput{}, 0, "add-model", "model-one")
	before := setupSnapshot(t, home)
	for _, test := range []struct {
		name  string
		args  []string
		input string
	}{
		{"duplicate provider", []string{"add-provider", "example"}, ""},
		{"bad URL", []string{"add-provider", "second", "--base-url", "http://api.example.invalid/v1"}, ""},
		{"canceled key", []string{"add-provider", "second", "--base-url", "https://api.example.invalid/v1"}, "\x03"},
		{"invalid key", []string{"add-provider", "second", "--base-url", "https://api.example.invalid/v1"}, "two words\n"},
		{"two key refs", []string{"add-provider", "second", "--api-key-env", "GATEWAY_SETUP_TEST_KEY", "--api-key-file", "/unused"}, ""},
		{"official custom URL", []string{"add-provider", "--official", "--base-url", "https://api.example.invalid/v1"}, ""},
		{"duplicate model", []string{"add-model", "model-one"}, ""},
		{"unknown provider", []string{"add-model", "model-two", "--provider", "missing"}, ""},
		{"invalid alias", []string{"add-model", "model-two", "--alias", "invalid alias"}, ""},
		{"invalid template", []string{"add-model", "model-two", "--template", "missing-template"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupCall(t, home, strings.NewReader(test.input), 2, test.args...)
			if !reflect.DeepEqual(before, setupSnapshot(t, home)) {
				t.Fatal("failed operation changed persisted configuration or credentials")
			}
		})
	}
}

func TestSetupFreshCancellationAndHelpDoNotCreateAHome(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want int
	}{
		{"cancel name", []string{"add-provider"}, 2},
		{"cancel key", []string{"add-provider", "example", "--base-url", "https://api.example.invalid/v1"}, 2},
		{"provider help", []string{"add-provider", "--help"}, 0},
		{"model help", []string{"add-model", "--help"}, 0},
		{"model before provider", []string{"add-model", "model-one"}, 2},
		{"official missing executable", []string{"add-provider", "--official"}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := setupTestHome(t)
			setupCall(t, home, strings.NewReader(""), test.want, test.args...)
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatal("canceled/help command created or changed a gateway home")
			}
		})
	}
}

func TestSetupSelectsAmongProvidersAndPreservesDefault(t *testing.T) {
	home := setupTestHome(t)
	setupAddKeyProvider(t, home, "alpha")
	setupAddKeyProvider(t, home, "beta")
	setupCall(t, home, strings.NewReader("2\n"), 0, "add-model", "model-one")
	cfg, err := LoadConfig(home)
	if err != nil || cfg.DefaultModel != "beta/model-one" || cfg.Models[cfg.DefaultModel].Provider != "beta" {
		t.Fatalf("numbered provider selection failed: %v", err)
	}
	setupCall(t, home, strings.NewReader("alpha\n"), 0, "add-model", "model-two")
	cfg, _ = LoadConfig(home)
	if cfg.Models["alpha/model-two"].Provider != "alpha" || cfg.DefaultModel != "beta/model-one" {
		t.Fatal("named provider selection or default preservation failed")
	}
	before := setupSnapshot(t, home)
	setupCall(t, home, strings.NewReader(""), 2, "add-model", "model-three")
	if !reflect.DeepEqual(before, setupSnapshot(t, home)) {
		t.Fatal("canceled provider selection changed configuration")
	}
}

func TestSetupBareCommandStartsGatewayAndReturnsCodexExitCode(t *testing.T) {
	t.Setenv("CODEX_GATEWAY_LIFECYCLE_HELPER", "1")
	t.Setenv("CODEX_GATEWAY_TEST_EXIT", "29")
	f := newLifecycleFixture(t)
	f.cleanupServer(t)
	fake := lifecycleFakeExecutable(t)
	t.Setenv("CODEX_GATEWAY_CODEX_BIN", fake)
	record := filepath.Join(t.TempDir(), "record.json")
	t.Setenv("CODEX_GATEWAY_TEST_RECORD", record)
	beforeConfig, _ := os.ReadFile(filepath.Join(f.cfg.CodexHome, "config.toml"))
	beforeAuth, _ := os.ReadFile(filepath.Join(f.cfg.CodexHome, "auth.json"))
	setupCall(t, f.home, &setupNoInput{}, 29)
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal("bare gateway command did not enter Codex")
	}
	var observed struct {
		Args []string          `json:"args"`
		Env  map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Env["CODEX_GATEWAY_TOKEN"] != lifecycleTestClient || !strings.Contains(strings.Join(observed.Args, "\n"), "example/model") {
		t.Fatal("bare gateway command omitted model/token configuration")
	}
	afterConfig, _ := os.ReadFile(filepath.Join(f.cfg.CodexHome, "config.toml"))
	afterAuth, _ := os.ReadFile(filepath.Join(f.cfg.CodexHome, "auth.json"))
	if !bytes.Equal(beforeConfig, afterConfig) || !bytes.Equal(beforeAuth, afterAuth) {
		t.Fatal("bare gateway command changed existing native Codex files")
	}
}
