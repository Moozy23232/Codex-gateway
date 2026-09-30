package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

type ModelTemplateOptions struct {
	Template        string
	NativeModel     string
	ContextWindow   int
	ReasoningEffort string
}

type ModelChoice struct {
	ID          string
	DisplayName string
}

const (
	modelListMaxBytes = 4 << 20
	modelListMaxItems = 10000
)

func validModelIdentifier(value string) bool {
	return value != "" && len(value) <= 512 && strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) == -1
}

func validateModelTemplateOptions(modelID string, options ModelTemplateOptions) error {
	if !validModelIdentifier(modelID) {
		return errors.New("model ID must be nonempty, at most 512 bytes, and contain no whitespace or control characters")
	}
	if options.Template != "" && !validModelIdentifier(options.Template) {
		return errors.New("invalid template identifier")
	}
	if options.NativeModel != "" && !validModelIdentifier(options.NativeModel) {
		return errors.New("invalid GPT model identifier")
	}
	if options.Template != "" && options.NativeModel != "" {
		return errors.New("choose either --gpt-model or the advanced --template option")
	}
	if options.ContextWindow < 0 || options.ContextWindow > 1_000_000_000 {
		return errors.New("context window must be between 1 and 1000000000 tokens")
	}
	if effort := options.ReasoningEffort; effort != "" {
		if len(effort) > 64 || strings.IndexFunc(effort, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_')
		}) != -1 {
			return errors.New("reasoning effort must be a short identifier without whitespace")
		}
	}
	return nil
}

// PrepareModelTemplate uses an exact native model or an explicit advanced
// template. Unknown model names never silently acquire invented capabilities.
// The returned template is stored separately from the model route; callers save
// their validated route with SaveConfig after this succeeds.
func PrepareModelTemplate(home string, cfg *Config, modelID string, options ModelTemplateOptions) (string, error) {
	if err := validateModelTemplateOptions(modelID, options); err != nil {
		return "", err
	}
	if cfg == nil {
		return "", errors.New("gateway configuration is required")
	}
	path := filepath.Join(home, "templates.json")
	catalog, err := TemplatesFrom(path)
	if err != nil {
		return "", err
	}
	wanted := modelID
	var source map[string]any
	if options.Template != "" {
		wanted = options.Template
		source = findModelTemplate(catalog, wanted)
		if source == nil {
			if native, err := discoverTemplates(cfg.CodexHome, ""); err == nil {
				source = findModelTemplate(native, wanted)
			}
		}
		if source == nil {
			return "", fmt.Errorf("unknown template %s; choose an imported template or omit --template", wanted)
		}
	} else {
		if options.NativeModel != "" {
			wanted = options.NativeModel
		}
		native, err := nativeModelsForSetup(home, cfg)
		if err != nil {
			return "", err
		}
		source = findModelTemplate(native, wanted)
		if source == nil || !nativeModelInfoValid(source) {
			if options.NativeModel == "" && !strings.HasPrefix(modelID, "gpt-") {
				return "", fmt.Errorf("%w: specify --gpt-model for %s", ErrModelNeedsGPTMapping, modelID)
			}
			return "", fmt.Errorf("no exact native capabilities for %s; select a known --gpt-model or update the native model catalog", wanted)
		}
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return "", errors.New("cannot copy model template")
	}
	var item map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&item); err != nil {
		return "", errors.New("cannot copy model template")
	}
	if options.ContextWindow > 0 {
		setTemplateContext(item, options.ContextWindow)
	}
	if effort := options.ReasoningEffort; effort != "" {
		if !templateSupportsEffort(item, effort) {
			return "", fmt.Errorf("template %s does not advertise reasoning effort %s", wanted, effort)
		}
		item["default_reasoning_level"] = effort
	}
	slug := wanted
	changedSource := false
	if existing := findModelTemplate(catalog, wanted); existing != nil {
		left, _ := json.Marshal(existing)
		right, _ := json.Marshal(item)
		changedSource = !bytes.Equal(left, right)
	}
	if changedSource || options.ContextWindow != 0 || options.ReasoningEffort != "" {
		// JSON map encoding sorts keys. Content-derived names preserve templates
		// referenced by existing routes and make repeated setup idempotent.
		encoded, err = json.Marshal(item)
		if err != nil {
			return "", errors.New("cannot encode model template")
		}
		digest := sha256.Sum256(encoded)
		slug = "gateway-template-" + hex.EncodeToString(digest[:16])
	}
	item["slug"] = slug
	if existing := findModelTemplate(catalog, slug); existing != nil {
		left, _ := json.Marshal(existing)
		right, _ := json.Marshal(item)
		if !bytes.Equal(left, right) {
			return "", errors.New("generated template name is already used by different metadata")
		}
		return slug, nil
	}
	catalog.Models = append(catalog.Models, item)
	if err := WriteJSON(path, catalog); err != nil {
		return "", err
	}
	return slug, nil
}

func findModelTemplate(catalog Catalog, slug string) map[string]any {
	for _, item := range catalog.Models {
		if item["slug"] == slug {
			return item
		}
	}
	return nil
}

func templateSupportsEffort(item map[string]any, effort string) bool {
	levels, _ := item["supported_reasoning_levels"].([]any)
	for _, level := range levels {
		entry, _ := level.(map[string]any)
		if entry["effort"] == effort {
			return true
		}
	}
	return false
}

func setTemplateContext(item map[string]any, window int) {
	item["context_window"] = window
	item["max_context_window"] = window
	limit := int(int64(window) * 7 / 8)
	if limit == 0 {
		limit = 1
	}
	item["auto_compact_token_limit"] = limit
}

// DiscoverProviderModels does not initialize or save configuration. Discovery
// failure leaves callers free to accept an exact model ID manually.
func DiscoverProviderModels(home string, cfg *Config, providerName string) ([]ModelChoice, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	provider, ok := cfg.Providers[providerName]
	if !ok {
		return nil, errors.New("unknown provider")
	}
	if provider.Auth == "codex" {
		return discoverOfficialModels(home, cfg, provider)
	}
	key, err := ResolveAPIKey(provider)
	if err != nil {
		return nil, err
	}
	client, err := newUpstreamClient(provider.Proxy)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	client.Timeout = 15 * time.Second
	request, err := http.NewRequest(http.MethodGet, strings.TrimRight(provider.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, errors.New("cannot construct provider model request")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("could not query provider models; enter the model ID manually")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model discovery returned HTTP %d; enter the model ID manually", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, modelListMaxBytes+1))
	if err != nil || len(data) > modelListMaxBytes {
		return nil, errors.New("provider model list is incomplete or exceeds the size limit")
	}
	return parseProviderModels(data)
}

func parseProviderModels(data []byte) ([]ModelChoice, error) {
	if err := checkJSON(data); err != nil {
		return nil, errors.New("provider returned an invalid model list")
	}
	var list struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &list); err != nil || list.Data == nil || len(list.Data) > modelListMaxItems {
		return nil, errors.New("provider must return a data array with at most 10000 models")
	}
	choices := make([]ModelChoice, 0, len(list.Data))
	for _, model := range list.Data {
		choices = append(choices, ModelChoice{ID: model.ID, DisplayName: model.DisplayName})
	}
	return cleanModelChoices(choices)
}

func cleanModelChoices(choices []ModelChoice) ([]ModelChoice, error) {
	seen := map[string]bool{}
	clean := make([]ModelChoice, 0, len(choices))
	for _, choice := range choices {
		if !validModelIdentifier(choice.ID) || seen[choice.ID] {
			continue
		}
		seen[choice.ID] = true
		if choice.DisplayName == "" || len(choice.DisplayName) > 256 || strings.IndexFunc(choice.DisplayName, unicode.IsControl) >= 0 {
			choice.DisplayName = choice.ID
		}
		clean = append(clean, choice)
	}
	if len(clean) == 0 {
		return nil, errors.New("provider returned no usable models; enter the model ID manually")
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i].ID < clean[j].ID })
	return clean, nil
}

func discoverOfficialModels(home string, cfg *Config, provider Provider) ([]ModelChoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	snapshot, err := loadOfficialModelSnapshot(ctx, home, cfg, provider)
	if err != nil {
		return nil, err
	}
	choices := []ModelChoice{}
	for _, item := range snapshot.Choices {
		if item.Hidden {
			continue
		}
		id := item.Model
		if id == "" {
			id = item.ID
		}
		choices = append(choices, ModelChoice{ID: id, DisplayName: item.DisplayName})
	}
	return cleanModelChoices(choices)
}
