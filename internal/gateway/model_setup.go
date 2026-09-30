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
	ContextWindow   int
	ReasoningEffort string
}

type ModelChoice struct {
	ID          string
	DisplayName string
}

const (
	genericContextWindow = 32000
	modelListMaxBytes    = 4 << 20
	modelListMaxItems    = 10000
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

// PrepareModelTemplate selects exact known metadata or creates a conservative
// text-only template. It never chooses another model's capabilities implicitly.
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
	if options.Template != "" {
		wanted = options.Template
	}
	source := findModelTemplate(catalog, wanted)
	inCatalog := source != nil
	if source == nil {
		// Codex may have refreshed its cache since this gateway was initialized.
		// A missing or invalid external cache does not prevent manual setup.
		if native, err := discoverTemplates(cfg.CodexHome, ""); err == nil {
			source = findModelTemplate(native, wanted)
		}
	}
	if source == nil && options.Template != "" {
		return "", fmt.Errorf("unknown template %s; choose an imported template or omit --template", options.Template)
	}
	if source != nil && inCatalog && options.ContextWindow == 0 && options.ReasoningEffort == "" {
		return wanted, nil
	}
	generic := source == nil
	if generic {
		source = genericModelTemplate(modelID)
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
		if generic {
			item["supported_reasoning_levels"] = []any{map[string]any{"effort": effort, "description": "Configured reasoning effort"}}
		} else if !templateSupportsEffort(item, effort) {
			return "", fmt.Errorf("template %s does not advertise reasoning effort %s", wanted, effort)
		}
		item["default_reasoning_level"] = effort
	}
	slug := wanted
	if generic || options.ContextWindow != 0 || options.ReasoningEffort != "" {
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

func genericModelTemplate(modelID string) map[string]any {
	item := map[string]any{
		"slug": modelID, "display_name": modelID,
		"description":             "Generic Responses model; capabilities and context budget can be configured.",
		"default_reasoning_level": "none", "supported_reasoning_levels": []any{},
		"shell_type": "shell_command", "visibility": "list", "supported_in_api": true, "priority": 0,
		"base_instructions": "You are a coding assistant. Follow the user instructions and use available tools when appropriate.",
		"input_modalities":  []any{"text"}, "support_verbosity": false,
		"supports_reasoning_summaries": false, "supports_reasoning_summary_parameter": false,
		"supports_parallel_tool_calls": false, "supports_search_tool": false,
		"supports_image_detail_original": false, "prefer_websockets": false,
		"apply_patch_tool_type": nil, "web_search_tool_type": "text",
		"truncation_policy":            map[string]any{"mode": "tokens", "limit": 8000},
		"experimental_supported_tools": []any{}, "use_responses_lite": false,
		"additional_speed_tiers": []any{}, "service_tiers": []any{},
	}
	setTemplateContext(item, genericContextWindow)
	return item
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
		return discoverOfficialModels(home, cfg)
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

func discoverOfficialModels(home string, cfg *Config) ([]ModelChoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	choices := []ModelChoice{}
	cursor := ""
	seenCursors := map[string]bool{}
	for page := 0; page < 100; page++ {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var response struct {
			Data []struct {
				ID          string `json:"id"`
				Model       string `json:"model"`
				DisplayName string `json:"displayName"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := nativeCodexRPC(ctx, home, cfg, "model/list", params, &response); err != nil {
			return nil, err
		}
		if response.Data == nil || len(response.Data)+len(choices) > modelListMaxItems {
			return nil, errors.New("native Codex returned an invalid or oversized model list")
		}
		for _, item := range response.Data {
			id := item.Model
			if id == "" {
				id = item.ID
			}
			choices = append(choices, ModelChoice{ID: id, DisplayName: item.DisplayName})
		}
		if response.NextCursor == nil || *response.NextCursor == "" {
			return cleanModelChoices(choices)
		}
		cursor = *response.NextCursor
		if seenCursors[cursor] {
			return nil, errors.New("native Codex returned a repeated model-list cursor")
		}
		seenCursors[cursor] = true
	}
	return nil, errors.New("native Codex model list exceeded the page limit")
}
