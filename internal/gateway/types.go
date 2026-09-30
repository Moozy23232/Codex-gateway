package gateway

// Version is overridden by release builds through the linker's -X option.
var Version = "0.2.0-dev"

type ListenConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type Config struct {
	Version                        int                 `json:"version"`
	Listen                         ListenConfig        `json:"listen"`
	CodexHome                      string              `json:"codex_home"`
	ClientAuth                     string              `json:"client_auth"`
	DefaultModel                   string              `json:"default_model,omitempty"`
	BootstrapProxy                 string              `json:"bootstrap_proxy,omitempty"`
	RetryInvalidEncryptedReasoning bool                `json:"retry_invalid_encrypted_reasoning"`
	Providers                      map[string]Provider `json:"providers"`
	Models                         map[string]Model    `json:"models"`
}

type Provider struct {
	BaseURL           string `json:"base_url"`
	Auth              string `json:"auth"`
	APIKeyEnv         string `json:"api_key_env,omitempty"`
	APIKeyFile        string `json:"api_key_file,omitempty"`
	Proxy             string `json:"proxy,omitempty"`
	AllowInsecureHTTP bool   `json:"allow_insecure_http,omitempty"`
}

type Model struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Template    string `json:"template"`
	DisplayName string `json:"display_name,omitempty"`
}

type Catalog struct {
	Models []map[string]any `json:"models"`
}
