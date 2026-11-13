package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// qoderModel is the provider model metadata exposed to the host.
type qoderModel struct {
	ID               string
	DisplayName      string
	Description      string
	InputTokenLimit  int64
	OutputTokenLimit int64
	InputModalities  []string
	Reasoning        bool
}

// defaultQoderModels is the model catalog registered for every Qoder auth.
var defaultQoderModels = []qoderModel{
	{ID: "auto", DisplayName: "Qoder Auto", Description: "Automatically picks the best available model.", InputTokenLimit: 180000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}, Reasoning: true},
	{ID: "ultimate", DisplayName: "Qoder Ultimate", Description: "Strongest reasoning tier.", InputTokenLimit: 1000000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}, Reasoning: true},
	{ID: "performance", DisplayName: "Qoder Performance", Description: "Balanced quality and speed tier.", InputTokenLimit: 1000000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}, Reasoning: true},
	{ID: "efficient", DisplayName: "Qoder Efficient", Description: "Fast, low-latency tier.", InputTokenLimit: 180000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}},
	{ID: "lite", DisplayName: "Qoder Lite", Description: "Lightweight tier.", InputTokenLimit: 180000, OutputTokenLimit: 32768, InputModalities: []string{"text"}},
	{ID: "qmodel", DisplayName: "Qwen3.7 Plus (Qoder)", Description: "Dedicated Qwen model slot.", InputTokenLimit: 1000000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}},
	{ID: "qmodel_latest", DisplayName: "Qwen3.7 Max (Qoder)", Description: "Latest dedicated Qwen model slot.", InputTokenLimit: 1000000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}},
	{ID: "dmodel", DisplayName: "DeepSeek V4 Pro (Qoder)", Description: "Dedicated DeepSeek Pro model slot.", InputTokenLimit: 1000000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}, Reasoning: true},
	{ID: "dfmodel", DisplayName: "DeepSeek V4 Flash (Qoder)", Description: "Dedicated DeepSeek Flash model slot.", InputTokenLimit: 1000000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}, Reasoning: true},
	{ID: "gm51model", DisplayName: "GLM 5.1 (Qoder)", Description: "Dedicated GLM model slot.", InputTokenLimit: 180000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}, Reasoning: true},
	{ID: "kmodel", DisplayName: "Kimi K2.6 (Qoder)", Description: "Dedicated Kimi model slot.", InputTokenLimit: 256000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}},
	{ID: "mmodel", DisplayName: "MiniMax M3 (Qoder)", Description: "Dedicated MiniMax model slot.", InputTokenLimit: 1000000, OutputTokenLimit: 32768, InputModalities: []string{"text", "image"}},
}

type qoderModelListResponse struct {
	Chat []qoderModelEntry `json:"chat"`
}

type qoderModelEntry struct {
	Key             string                        `json:"key"`
	Enable          bool                          `json:"enable"`
	DisplayName     string                        `json:"display_name"`
	MaxInputTokens  int64                         `json:"max_input_tokens"`
	MaxOutputTokens int64                         `json:"max_output_tokens"`
	ContextConfig   map[string]qoderContextConfig `json:"context_config"`
	IsVL            bool                          `json:"is_vl"`
	IsReasoning     bool                          `json:"is_reasoning"`
	ThinkingConfig  json.RawMessage               `json:"thinking_config"`
}

type qoderContextConfig struct {
	TokenCount int64 `json:"token_count"`
}

func parseQoderModels(raw []byte) ([]qoderModel, error) {
	var response qoderModelListResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, fmt.Errorf("decode qoder model list: %w", errUnmarshal)
	}
	models := make([]qoderModel, 0, len(response.Chat))
	for _, entry := range response.Chat {
		key := strings.TrimSpace(entry.Key)
		if key == "" || !entry.Enable {
			continue
		}
		inputLimit := entry.MaxInputTokens
		for _, context := range entry.ContextConfig {
			if context.TokenCount > inputLimit {
				inputLimit = context.TokenCount
			}
		}
		if inputLimit <= 0 {
			inputLimit = 180000
		}
		outputLimit := entry.MaxOutputTokens
		if outputLimit <= 0 {
			outputLimit = 32768
		}
		displayName := strings.TrimSpace(entry.DisplayName)
		if displayName == "" {
			displayName = key
		}
		modalities := []string{"text"}
		if entry.IsVL {
			modalities = append(modalities, "image")
		}
		thinkingConfig := strings.TrimSpace(string(entry.ThinkingConfig))
		models = append(models, qoderModel{
			ID:               key,
			DisplayName:      displayName,
			InputTokenLimit:  inputLimit,
			OutputTokenLimit: outputLimit,
			InputModalities:  modalities,
			Reasoning:        entry.IsReasoning || thinkingConfig != "" && thinkingConfig != "null" && thinkingConfig != "false",
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("qoder model list contains no enabled chat models")
	}
	return models, nil
}

// registeredModels returns the effective model catalog, honoring the optional
// models override from the plugin config.
func registeredModels(cfg pluginConfig) []qoderModel {
	if len(cfg.Models) == 0 {
		return defaultQoderModels
	}
	models := make([]qoderModel, 0, len(cfg.Models))
	for _, id := range cfg.Models {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		models = append(models, qoderModel{
			ID:               id,
			DisplayName:      id,
			InputTokenLimit:  180000,
			OutputTokenLimit: 32768,
			InputModalities:  []string{"text", "image"},
		})
	}
	return models
}

func selectConfiguredModels(cfg pluginConfig, discovered []qoderModel) []qoderModel {
	if len(cfg.Models) == 0 {
		return discovered
	}
	byID := make(map[string]qoderModel, len(discovered))
	for _, model := range discovered {
		byID[model.ID] = model
	}
	selected := registeredModels(cfg)
	for index, model := range selected {
		if discoveredModel, ok := byID[model.ID]; ok {
			selected[index] = discoveredModel
		}
	}
	return selected
}

// modelInfos converts the catalog to the host model registration shape.
func modelInfos(models []qoderModel) []pluginapi.ModelInfo {
	infos := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		info := pluginapi.ModelInfo{
			ID:                         model.ID,
			Object:                     "model",
			OwnedBy:                    pluginIdentifier,
			Type:                       pluginIdentifier,
			DisplayName:                model.DisplayName,
			Name:                       model.ID,
			Description:                model.Description,
			InputTokenLimit:            model.InputTokenLimit,
			OutputTokenLimit:           model.OutputTokenLimit,
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              model.InputTokenLimit,
			MaxCompletionTokens:        model.OutputTokenLimit,
			SupportedInputModalities:   append([]string(nil), model.InputModalities...),
			SupportedOutputModalities:  []string{"text"},
		}
		if model.Reasoning {
			info.Thinking = &pluginapi.ThinkingSupport{DynamicAllowed: true}
		}
		infos = append(infos, info)
	}
	return infos
}

// handleModelStatic implements model.static.
func handleModelStatic(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ModelResponse{
		Provider: pluginIdentifier,
		Models:   modelInfos(registeredModels(loadedConfig())),
	})
}

// handleModelForAuth discovers the live Qoder catalog. Credential, network,
// status, and response failures all fall back to the built-in catalog so model
// registration cannot prevent CPA from starting.
func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	fallback := modelInfos(registeredModels(cfg))
	creds, errCredentials := resolveQoderCredentials(req.StorageJSON, cfg)
	if errCredentials != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginIdentifier, Models: fallback})
	}
	requestURL := cfg.BaseURL + "/algo/api/v2/model/list"
	headers, errHeaders := buildCosyHeaders(nil, requestURL, creds)
	if errHeaders != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginIdentifier, Models: fallback})
	}
	headers["Accept"] = []string{"application/json"}
	response, errRequest := hostHTTPDo(hostHTTPRequest{
		Method:  http.MethodGet,
		URL:     requestURL,
		Headers: headers,
	})
	if errRequest != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginIdentifier, Models: fallback})
	}
	models, errModels := parseQoderModels(response.Body)
	if errModels != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginIdentifier, Models: fallback})
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: pluginIdentifier,
		Models:   modelInfos(selectConfiguredModels(cfg, models)),
	})
}
