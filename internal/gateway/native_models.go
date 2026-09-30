package gateway

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The unmodified public Codex 0.159.2 catalog is an offline capability source,
// not an account entitlement list. See native_models_source.txt and licenses.
//
//go:embed native_models_builtin.json
var bundledNativeModels []byte

//go:embed native_models_source.txt
var nativeModelSource string

//go:embed native_models_LICENSE.txt
var nativeModelLicense string

//go:embed native_models_NOTICE.txt
var nativeModelNotice string

const bundledNativeModelsVersion = "0.159.2"

var ErrModelNeedsGPTMapping = errors.New("select the GPT model used by this provider")

func NativeModelNotices() string {
	return strings.Join([]string{nativeModelSource, nativeModelLicense, nativeModelNotice}, "\n\n")
}

func decodeNativeModels(data []byte) (Catalog, error) {
	var catalog Catalog
	if len(data) > modelListMaxBytes || checkJSON(data) != nil {
		return catalog, errors.New("invalid native model catalog")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&catalog); err != nil || catalog.Models == nil || len(catalog.Models) > modelListMaxItems {
		return Catalog{}, errors.New("native model catalog must contain a bounded models list")
	}
	seen := map[string]bool{}
	for _, item := range catalog.Models {
		slug, _ := item["slug"].(string)
		if !validModelIdentifier(slug) || seen[slug] {
			return Catalog{}, errors.New("native model catalog contains invalid or repeated IDs")
		}
		seen[slug] = true
	}
	return catalog, nil
}

func readNativeModels(path string) (Catalog, error) {
	file, err := os.Open(path)
	if err != nil {
		return Catalog{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, modelListMaxBytes+1))
	if err != nil {
		return Catalog{}, err
	}
	return decodeNativeModels(data)
}

func nativeModelInfoValid(item map[string]any) bool {
	slug, _ := item["slug"].(string)
	if !validModelIdentifier(slug) || strings.Contains(slug, "/") || strings.HasPrefix(slug, "gateway-template-") || strings.HasPrefix(slug, "native-template-") {
		return false
	}
	window, ok := item["context_window"].(json.Number)
	if !ok {
		return false
	}
	n, err := window.Int64()
	if err != nil || n <= 0 || n > 1_000_000_000 {
		return false
	}
	levels, ok := item["supported_reasoning_levels"].([]any)
	if !ok || len(levels) == 0 {
		return false
	}
	defaultEffort, _ := item["default_reasoning_level"].(string)
	if !templateSupportsEffort(item, defaultEffort) {
		return false
	}
	modalities, ok := item["input_modalities"].([]any)
	if !ok || len(modalities) == 0 {
		return false
	}
	_, hasMessages := item["model_messages"].(map[string]any)
	base, _ := item["base_instructions"].(string)
	return hasMessages || base != ""
}

func nativeModelsForSetup(home string, cfg *Config) (Catalog, error) {
	catalog, err := decodeNativeModels(bundledNativeModels)
	if err != nil {
		return Catalog{}, fmt.Errorf("bundled native metadata is invalid: %w", err)
	}
	lookup := map[string]int{}
	for i, item := range catalog.Models {
		lookup[item["slug"].(string)] = i
	}
	paths := []string{}
	if home != "" {
		paths = append(paths, filepath.Join(home, "official-models-cache.json"))
	}
	if cfg != nil && cfg.CodexHome != "" {
		// Deliberately do not follow the user's model_catalog_json: that file
		// can be an alias catalog produced by a gateway or another integration.
		paths = append(paths, filepath.Join(cfg.CodexHome, "models_cache.json"))
	}
	modified := map[string]int64{}
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil {
			modified[path] = info.ModTime().UnixNano()
		}
	}
	sort.SliceStable(paths, func(i, j int) bool { return modified[paths[i]] < modified[paths[j]] })
	for _, path := range paths {
		incoming, err := readNativeModels(path)
		if err != nil {
			continue // A missing/offline/malformed cache never removes public data.
		}
		for _, item := range incoming.Models {
			if !nativeModelInfoValid(item) {
				continue
			}
			slug := item["slug"].(string)
			if i, ok := lookup[slug]; ok {
				catalog.Models[i] = item
			} else {
				lookup[slug] = len(catalog.Models)
				catalog.Models = append(catalog.Models, item)
			}
		}
	}
	return catalog, nil
}

// GetNativeModelChoices is local-only. API-key users never need a ChatGPT login
// to select exact GPT capabilities for a provider's nonstandard model name.
func GetNativeModelChoices(home string, cfg *Config) ([]ModelChoice, error) {
	catalog, err := nativeModelsForSetup(home, cfg)
	if err != nil {
		return nil, err
	}
	choices := []ModelChoice{}
	for _, item := range catalog.Models {
		slug := item["slug"].(string)
		if !strings.HasPrefix(slug, "gpt-") || !nativeModelInfoValid(item) {
			continue
		}
		name, _ := item["display_name"].(string)
		choices = append(choices, ModelChoice{ID: slug, DisplayName: name})
	}
	return cleanModelChoices(choices)
}
