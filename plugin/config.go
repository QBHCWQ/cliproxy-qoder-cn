package main

import (
	"encoding/json"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const (
	// pluginIdentifier is the stable plugin, provider, and executor key.
	pluginIdentifier = "qoder"

	// defaultBaseURL is the Qoder global gateway used for model discovery and
	// chat generation.
	defaultBaseURL = "https://api3.qoder.sh"

	// defaultOpenAPIBaseURL is the Qoder global OpenAPI service used for PAT
	// exchange and user information.
	defaultOpenAPIBaseURL = "https://openapi.qoder.sh"
	// chatCompletionsFormat is the executor input/output protocol declared to the host.
	chatCompletionsFormat = "chat-completions"
)

// lifecycleRequest mirrors the host RPC envelope for plugin.register and
// plugin.reconfigure. ConfigYAML carries the raw per-plugin YAML map from
// plugins.configs.<pluginID>.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

// pluginConfig holds the qoder-proxy plugin configuration.
type pluginConfig struct {
	Enabled        bool              `yaml:"enabled"`
	BaseURL        string            `yaml:"base_url"`
	OpenAPIBaseURL string            `yaml:"openapi_base_url"`
	Models         []string          `yaml:"models"`
	ModelMap       map[string]string `yaml:"model_map"`
	MachineID      string            `yaml:"machine_id"`
}

var (
	configMu     sync.RWMutex
	activeConfig = defaultPluginConfig()
)

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:        true,
		BaseURL:        defaultBaseURL,
		OpenAPIBaseURL: defaultOpenAPIBaseURL,
		ModelMap:       map[string]string{},
	}
}

// configure parses the lifecycle request and stores the resulting config.
func configure(raw []byte) error {
	cfg := defaultPluginConfig()
	if len(raw) > 0 {
		var req lifecycleRequest
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
		if len(req.ConfigYAML) > 0 {
			if errDecode := yaml.Unmarshal(req.ConfigYAML, &cfg); errDecode != nil {
				return errDecode
			}
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	cfg.OpenAPIBaseURL = strings.TrimRight(strings.TrimSpace(cfg.OpenAPIBaseURL), "/")
	if cfg.OpenAPIBaseURL == "" {
		cfg.OpenAPIBaseURL = defaultOpenAPIBaseURL
	}
	cfg.Models = normalizeStringList(cfg.Models)
	if cfg.ModelMap == nil {
		cfg.ModelMap = map[string]string{}
	}
	cfg.MachineID = strings.TrimSpace(cfg.MachineID)

	configMu.Lock()
	activeConfig = cfg
	configMu.Unlock()
	return nil
}

func loadedConfig() pluginConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return activeConfig
}

func normalizeStringList(items []string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

// mapModel resolves a client-requested model to the upstream Qoder model name.
// A "qoder/" prefix (CPA auth model prefix) is stripped first, then the
// configured model_map aliases are applied.
func mapModel(cfg pluginConfig, requested string) string {
	model := strings.TrimSpace(requested)
	if rest, ok := strings.CutPrefix(model, pluginIdentifier+"/"); ok {
		model = rest
	}
	if mapped, ok := cfg.ModelMap[model]; ok && strings.TrimSpace(mapped) != "" {
		return strings.TrimSpace(mapped)
	}
	return model
}
