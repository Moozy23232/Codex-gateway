package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

type OfficialModelSyncReport struct {
	Added   int
	Updated int
	Removed int
	Cached  bool
}

type nativeModelChoice struct {
	ID          string `json:"id"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName"`
	Hidden      bool   `json:"hidden"`
	IsDefault   bool   `json:"isDefault"`
}

type officialModelSnapshot struct {
	Models        []map[string]any    `json:"models"`
	Choices       []nativeModelChoice `json:"choices"`
	ClientVersion string              `json:"client_version"`
	AccountScope  string              `json:"account_scope"`
	FetchedAt     time.Time           `json:"fetched_at"`
	Cached        bool                `json:"-"`
}

// SyncOfficialModels changes only managed official routes and their templates.
// The caller persists cfg with SaveConfig. Offline refreshes preserve a usable
// prior directory, and manual routes/default choices are never overwritten.
func SyncOfficialModels(parent context.Context, home string, cfg *Config) (OfficialModelSyncReport, error) {
	ctx, cancel := context.WithTimeout(parent, nativeRPCTimeout)
	defer cancel()
	return syncOfficialModels(ctx, home, cfg, loadOfficialModelSnapshot)
}

func syncOfficialModels(ctx context.Context, home string, cfg *Config, load func(context.Context, string, *Config, Provider) (officialModelSnapshot, error)) (OfficialModelSyncReport, error) {
	var report OfficialModelSyncReport
	if cfg == nil {
		return report, errors.New("gateway configuration is required")
	}
	names := []string{}
	for name, provider := range cfg.Providers {
		if provider.AutoModels {
			if provider.Auth != "codex" || strings.TrimRight(provider.BaseURL, "/") != officialBaseURL {
				return report, errors.New("automatic model discovery requires the fixed official provider")
			}
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return report, nil
	}
	sort.Strings(names)
	snapshot, err := load(ctx, home, cfg, cfg.Providers[names[0]])
	if err != nil {
		if managedOfficialModelsUsable(home, cfg) {
			report.Cached = true
			return report, nil
		}
		return report, err
	}
	report.Cached = snapshot.Cached
	templates, err := TemplatesFrom(filepath.Join(home, "templates.json"))
	if err != nil {
		return report, err
	}
	models := make(map[string]Model, len(cfg.Models)+len(snapshot.Choices))
	visible := map[string]bool{}
	for _, choice := range snapshot.Choices {
		if !choice.Hidden {
			id := choice.Model
			if id == "" {
				id = choice.ID
			}
			visible[id] = true
		}
	}
	defaultModel := cfg.DefaultModel
	for alias, model := range cfg.Models {
		provider := cfg.Providers[model.Provider]
		if !snapshot.Cached && model.Managed && provider.AutoModels && provider.Auth == "codex" && !visible[model.Model] {
			report.Removed++
			if defaultModel == alias {
				defaultModel = ""
			}
			continue
		}
		models[alias] = model
	}
	for _, providerName := range names {
		for _, choice := range snapshot.Choices {
			if choice.Hidden {
				continue
			}
			id := choice.Model
			if id == "" {
				id = choice.ID
			}
			item := findModelTemplate(Catalog{Models: snapshot.Models}, id)
			if !nativeModelInfoValid(item) {
				return OfficialModelSyncReport{}, fmt.Errorf("official model %s has no complete native capabilities", id)
			}
			alias := officialModelAlias(models, providerName, id)
			if alias == "" {
				continue // Both conventional names belong to manual routes.
			}
			if existing, ok := models[alias]; ok && !existing.Managed {
				if defaultModel == "" && choice.IsDefault {
					defaultModel = alias
				}
				continue
			}
			encoded, _ := json.Marshal(item)
			digest := sha256.Sum256(encoded)
			slug := "native-template-" + hex.EncodeToString(digest[:16])
			if findModelTemplate(templates, slug) == nil {
				copy := make(map[string]any, len(item))
				for key, value := range item {
					copy[key] = value
				}
				copy["slug"] = slug
				templates.Models = append(templates.Models, copy)
			}
			model := Model{Provider: providerName, Model: id, NativeModel: id, Template: slug, DisplayName: choice.DisplayName, Managed: true}
			if existing, ok := models[alias]; !ok {
				report.Added++
			} else if !reflect.DeepEqual(existing, model) {
				report.Updated++
			}
			models[alias] = model
			if defaultModel == "" && choice.IsDefault {
				defaultModel = alias
			}
		}
	}
	if defaultModel == "" {
		aliases := make([]string, 0, len(models))
		for alias := range models {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		if len(aliases) > 0 {
			defaultModel = aliases[0]
		}
	}
	if report.Added != 0 || report.Updated != 0 {
		if err := WriteJSON(filepath.Join(home, "templates.json"), templates); err != nil {
			return OfficialModelSyncReport{}, err
		}
	}
	if !snapshot.Cached && snapshot.AccountScope != "" {
		if err := WriteJSON(filepath.Join(home, "official-models-cache.json"), snapshot); err != nil {
			return OfficialModelSyncReport{}, err
		}
	}
	cfg.Models, cfg.DefaultModel = models, defaultModel
	return report, nil
}

func officialModelAlias(models map[string]Model, provider, id string) string {
	aliases := []string{}
	for alias, model := range models {
		if model.Provider == provider && model.Model == id {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	if len(aliases) > 0 {
		return aliases[0]
	}
	for _, alias := range []string{id, provider + "/" + id} {
		if _, exists := models[alias]; !exists {
			return alias
		}
	}
	return ""
}

func managedOfficialModelsUsable(home string, cfg *Config) bool {
	credential, err := readOfficialCredential(cfg)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, "official-models-cache.json"))
	var cached struct {
		AccountScope string `json:"account_scope"`
	}
	if err != nil || len(data) > modelListMaxBytes || json.Unmarshal(data, &cached) != nil || cached.AccountScope != officialModelAccountScope(cfg, credential.accountID) {
		return false
	}
	templates, err := TemplatesFrom(filepath.Join(home, "templates.json"))
	if err != nil {
		return false
	}
	for _, model := range cfg.Models {
		if p := cfg.Providers[model.Provider]; model.Managed && p.Auth == "codex" && p.AutoModels && findModelTemplate(templates, model.Template) != nil {
			return true
		}
	}
	return false
}

func officialModelAccountScope(cfg *Config, accountID string) string {
	scope := sha256.Sum256([]byte(cfg.CodexHome + "\x00" + accountID))
	return hex.EncodeToString(scope[:])
}

func loadOfficialModelSnapshot(ctx context.Context, home string, cfg *Config, provider Provider) (officialModelSnapshot, error) {
	var snapshot officialModelSnapshot
	auth := newOfficialAuth(home, cfg)
	credential, err := auth.credential(ctx, false, "")
	if err != nil {
		return snapshot, err
	}
	executable, err := nativeCodexExecutablePath()
	if err != nil {
		return snapshot, err
	}
	versionOutput, err := exec.CommandContext(ctx, executable, "--version").Output()
	if err != nil {
		return snapshot, errors.New("cannot read the installed native Codex version")
	}
	match := regexp.MustCompile(`\b([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)\b`).FindSubmatch(versionOutput)
	if len(match) != 2 {
		return snapshot, errors.New("native Codex returned an unrecognized version")
	}
	version := string(match[1])
	client, err := newUpstreamClient(provider.Proxy)
	if err != nil {
		return snapshot, err
	}
	defer client.CloseIdleConnections()
	client.Timeout = 15 * time.Second
	catalog, err := fetchOfficialModelCatalog(ctx, client, auth, credential, version)
	if err != nil {
		return snapshot, err
	}
	choices, err := nativeOfficialModelChoices(ctx, home, cfg, catalog)
	if err != nil {
		return snapshot, err
	}
	snapshot = officialModelSnapshot{Models: catalog.Models, Choices: choices, ClientVersion: version, AccountScope: officialModelAccountScope(cfg, credential.accountID), FetchedAt: time.Now().UTC()}
	return snapshot, nil
}

// This is the same fixed /models endpoint and client_version query used by the
// public native models_endpoint.rs. OAuth refresh and workspace restrictions
// remain in officialAuth; this function never implements an OAuth exchange.
func fetchOfficialModelCatalog(ctx context.Context, client *http.Client, auth *officialAuth, credential officialCredential, version string) (Catalog, error) {
	for attempt := 0; attempt < 2; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, officialBaseURL+"/models?client_version="+url.QueryEscape(version), nil)
		if err != nil {
			return Catalog{}, errors.New("cannot construct official model discovery request")
		}
		credential.apply(request.Header)
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return Catalog{}, errors.New("official model discovery is unavailable; retry when online")
		}
		if response.StatusCode == http.StatusUnauthorized && attempt == 0 {
			response.Body.Close()
			credential, err = auth.credential(ctx, true, credential.token)
			if err != nil {
				return Catalog{}, err
			}
			continue
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return Catalog{}, fmt.Errorf("official model discovery returned HTTP %d", response.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, modelListMaxBytes+1))
		response.Body.Close()
		if err != nil || len(data) > modelListMaxBytes {
			return Catalog{}, errors.New("official model catalog is incomplete or exceeds the size limit")
		}
		return decodeNativeModels(data)
	}
	return Catalog{}, errors.New("official model discovery authentication failed")
}

func nativeOfficialModelChoices(ctx context.Context, home string, cfg *Config, catalog Catalog) ([]nativeModelChoice, error) {
	file, err := os.CreateTemp(home, ".official-models-*.json")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := json.NewEncoder(file).Encode(catalog); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	choices := []nativeModelChoice{}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		command, err := nativeCodexCommand(ctx, home, cfg, "-c", "model_catalog_json="+lifecycleJSONString(path), "app-server")
		if err != nil {
			return nil, err
		}
		var response struct {
			Data       []nativeModelChoice `json:"data"`
			NextCursor *string             `json:"nextCursor"`
		}
		if err := runNativeCodexRPC(ctx, command, "model/list", params, &response); err != nil {
			return nil, err
		}
		if response.Data == nil || len(choices)+len(response.Data) > modelListMaxItems {
			return nil, errors.New("native Codex returned an invalid model list")
		}
		choices = append(choices, response.Data...)
		if response.NextCursor == nil || *response.NextCursor == "" {
			return choices, nil
		}
		cursor = *response.NextCursor
		if seen[cursor] {
			return nil, errors.New("native Codex returned a repeated model cursor")
		}
		seen[cursor] = true
	}
	return nil, errors.New("native Codex model list exceeded the page limit")
}
