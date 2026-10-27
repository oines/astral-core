// Package integration combines reusable provider contracts, runtime catalogs,
// and model configuration. It has no dependency on an application's storage or UI.
package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
)

type ProviderConfig struct {
	ConnectionScope  string                   `json:"connection_scope,omitempty"`
	PresetID         string                   `json:"preset_id,omitempty"`
	Protocol         string                   `json:"protocol"`
	Dialect          string                   `json:"dialect"`
	BaseURL          string                   `json:"base_url"`
	ReasoningFormat  models.ReasoningFormat   `json:"reasoning_format,omitempty"`
	ReasoningMapping *models.ReasoningMapping `json:"reasoning_mapping,omitempty"`
}

type RecommendationSource struct {
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Catalog never persists an application's inventory. List succeeds only when
// the full authenticated directory has been fetched and validated.
type Catalog struct {
	HTTPClient *http.Client
	Public     *ModelCatalog
}

func invalid(message string) error { return fmt.Errorf("%w: %s", core.ErrInvalid, message) }
func unauthorized(message string) error {
	return &core.ModelError{Code: "authentication", Message: message}
}

type ModelValues struct {
	ContextWindow          int                      `json:"context_window"`
	MaxInputTokens         int                      `json:"max_input_tokens,omitempty"`
	MaxOutputTokens        int                      `json:"max_output_tokens"`
	ToolCalling            string                   `json:"tool_calling,omitempty"`
	Images                 string                   `json:"images,omitempty"`
	Streaming              *bool                    `json:"streaming,omitempty"`
	Reasoning              json.RawMessage          `json:"reasoning,omitempty"`
	ReasoningFormat        models.ReasoningFormat   `json:"reasoning_format,omitempty"`
	ReasoningMapping       *models.ReasoningMapping `json:"reasoning_mapping,omitempty"`
	ReasoningReplay        string                   `json:"reasoning_replay,omitempty"`
	RequireReasoningReplay bool                     `json:"require_reasoning_replay,omitempty"`
	ImageTokenBudget       int                      `json:"image_token_budget,omitempty"`
	Defaults               core.GenerationOptions   `json:"defaults"`
	InputPrice             *float64                 `json:"input_price,omitempty"`
	OutputPrice            *float64                 `json:"output_price,omitempty"`
}

// EffectiveValues applies explicit top-level overrides, including null, false,
// zero and empty lists. An override is removed to restore its recommendation.
// Token limits may be unknown (zero); bounded agent execution validates its
// complete budget separately, after configuration and controls are resolved.
func EffectiveValues(recommendations, overrides map[string]json.RawMessage) (ModelValues, map[string]string, error) {
	fields := map[string]json.RawMessage{}
	sources := map[string]string{}
	for key, value := range recommendations {
		fields[key] = value
		sources[key] = "recommended"
	}
	for key, value := range overrides {
		fields[key] = value
		sources[key] = "override"
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return ModelValues{}, nil, err
	}
	var values ModelValues
	if err = json.Unmarshal(data, &values); err != nil {
		return values, nil, err
	}
	if values.ContextWindow < 0 || values.MaxOutputTokens < 0 || values.MaxInputTokens < 0 {
		return values, nil, invalid("model token limits must not be negative")
	}
	for _, capability := range []string{values.ToolCalling, values.Images} {
		if capability != "" && capability != "unknown" && capability != "supported" && capability != "unsupported" {
			return values, nil, invalid("invalid capability status")
		}
	}
	if values.ImageTokenBudget < 0 {
		return values, nil, invalid("image token budget cannot be negative")
	}
	if err = values.Defaults.Validate(); err != nil {
		return values, nil, err
	}
	return values, sources, nil
}

type ModelConfig struct {
	ModelID              string
	Protocol             string
	Dialect              string
	RecommendationTarget *RecommendationTarget
	Recommendations      map[string]json.RawMessage
	Overrides            map[string]json.RawMessage
}

type Resolved struct {
	Protocol string
	Config   models.Config
	Values   ModelValues
	Sources  map[string]string
	Controls models.ReasoningControls
}

// Route resolves only explicit configuration and provider API contracts. The
// remote model identifier never selects a protocol or a dialect.
func Route(provider ProviderConfig, model ModelConfig) (protocol, dialect, baseURL string, err error) {
	protocol, dialect, baseURL = model.Protocol, model.Dialect, provider.BaseURL
	if protocol == "" {
		protocol = provider.Protocol
	}
	if dialect == "" {
		dialect = provider.Dialect
	}
	switch protocol {
	case "anthropic-messages":
		dialect = "anthropic"
	case "google-generative-ai":
		dialect = "google"
	case "openai-responses":
		if dialect != "deepseek" {
			dialect = "openai"
		}
	case "openai-completions":
		if dialect == "anthropic" || dialect == "google" {
			dialect = "compatible"
		}
	default:
		return "", "", "", invalid("unsupported model protocol")
	}
	if strings.HasPrefix(provider.PresetID, "opencode") {
		switch protocol {
		case "anthropic-messages":
			baseURL += "/anthropic/v1"
		case "google-generative-ai":
			baseURL += "/google/v1beta"
		default:
			baseURL += "/openai/v1"
		}
	}
	return protocol, dialect, ProtocolBaseURL(provider.PresetID, protocol, baseURL), nil
}

// Resolve produces one configuration for controls, validation and invocation.
// The host adds credential references and its HTTP transport after resolution.
func Resolve(provider ProviderConfig, model ModelConfig, spec core.ModelSpec) (Resolved, error) {
	result := Resolved{}
	model, err := RebaseModelConfig(provider, model)
	if err != nil {
		return result, err
	}
	values, sources, err := EffectiveValues(model.Recommendations, model.Overrides)
	if err != nil {
		return result, err
	}
	protocol, dialect, baseURL, err := Route(provider, model)
	if err != nil {
		return result, err
	}
	var diagnostics []models.ReasoningDiagnostic
	values.Reasoning, diagnostics, err = ResolveReasoningSemantics(provider, protocol, values.Reasoning)
	if err != nil {
		return result, err
	}
	spec.ContextWindow, spec.MaxInputTokens, spec.MaxOutputTokens = values.ContextWindow, values.MaxInputTokens, values.MaxOutputTokens
	config := models.Config{ToolCalling: values.ToolCalling, Images: values.Images, Spec: spec, Model: model.ModelID, BaseURL: baseURL, Dialect: models.Dialect(dialect), Streaming: values.Streaming, ReasoningReplay: values.ReasoningReplay, RequireReasoningReplay: values.RequireReasoningReplay, ImageTokenBudget: values.ImageTokenBudget}
	if len(values.Reasoning) > 0 && !isNull(values.Reasoning) {
		config.Reasoning = &models.ReasoningCapabilities{}
		if json.Unmarshal(values.Reasoning, config.Reasoning) != nil {
			return result, invalid("invalid reasoning capabilities")
		}
	}
	config.ReasoningFormat = values.ReasoningFormat
	if config.ReasoningFormat == "" && protocol == provider.Protocol {
		config.ReasoningFormat = provider.ReasoningFormat
	}
	if config.ReasoningFormat == "" {
		config.ReasoningFormat = ReasoningFormat(provider.PresetID, protocol)
	}
	config.ReasoningMapping = values.ReasoningMapping
	if config.ReasoningMapping == nil && config.ReasoningFormat == models.ReasoningFormatCustom {
		config.ReasoningMapping = provider.ReasoningMapping
	}
	if config.ReasoningMapping != nil && config.ReasoningFormat != models.ReasoningFormatCustom {
		return result, invalid("a reasoning parameter mapping requires the custom reasoning format")
	}
	if protocol == "openai-responses" {
		config.ResponsesCompatibility = ResponsesCompatibility(provider.PresetID)
	}
	config.Codex = provider.PresetID == "codex"
	config.RequireAdaptiveEffort = provider.PresetID == "fireworks" && protocol == "anthropic-messages"
	controls, err := models.ResolveReasoningControls(protocol, config)
	if err != nil {
		return result, err
	}
	controls.Diagnostics = append(controls.Diagnostics, diagnostics...)
	result = Resolved{Protocol: protocol, Config: config, Values: values, Sources: sources, Controls: controls}
	return result, nil
}

// Options merges request/session overrides onto model defaults. Reasoning nil
// inherits defaults; an explicit empty object requests the API's own default.
func Options(defaults, overrides core.GenerationOptions, reasoning *core.ReasoningOptions) (core.GenerationOptions, error) {
	data, err := json.Marshal(defaults)
	if err != nil {
		return core.GenerationOptions{}, err
	}
	var merged core.GenerationOptions
	if err = json.Unmarshal(data, &merged); err != nil {
		return merged, err
	}
	if overrides.Temperature != nil {
		merged.Temperature = overrides.Temperature
	}
	if overrides.TopP != nil {
		merged.TopP = overrides.TopP
	}
	if overrides.Stop != nil {
		merged.Stop = append([]string{}, overrides.Stop...)
	}
	if overrides.Seed != nil {
		merged.Seed = overrides.Seed
	}
	if overrides.FrequencyPenalty != nil {
		merged.FrequencyPenalty = overrides.FrequencyPenalty
	}
	if overrides.PresencePenalty != nil {
		merged.PresencePenalty = overrides.PresencePenalty
	}
	if overrides.Reasoning.Enabled != nil || overrides.Reasoning.Effort != "" || overrides.Reasoning.BudgetTokens != nil {
		merged.Reasoning = overrides.Reasoning
	}
	if overrides.ToolChoice.Mode != "" {
		merged.ToolChoice = overrides.ToolChoice
	}
	if overrides.ResponseFormat != nil {
		merged.ResponseFormat = overrides.ResponseFormat
	}
	for namespace, value := range overrides.ProviderOptions {
		if merged.ProviderOptions == nil {
			merged.ProviderOptions = map[string]json.RawMessage{}
		}
		merged.ProviderOptions[namespace] = append(json.RawMessage(nil), value...)
	}
	if reasoning != nil {
		merged.Reasoning = *reasoning
	}
	if err := merged.Validate(); err != nil {
		return merged, err
	}
	// Both defaults and caller overrides may be reused or edited later.
	data, err = json.Marshal(merged)
	if err != nil {
		return merged, err
	}
	var owned core.GenerationOptions
	if err = json.Unmarshal(data, &owned); err != nil {
		return merged, err
	}
	return owned, nil
}

// Headers applies provider-specific authentication conventions without exposing
// them to a host. OAuth authorization is supplied by models/auth's transport.
func Headers(provider ProviderConfig, key string, headers map[string]string) map[string]string {
	result := map[string]string{"User-Agent": "astral-core"}
	for name, value := range headers {
		result[name] = value
	}
	if key != "" && (strings.HasPrefix(provider.PresetID, "opencode") || isSelfHostedGateway(provider.PresetID) || provider.PresetID == "moonshot" || provider.PresetID == "ollama") {
		result["Authorization"] = "Bearer " + key
	}
	return result
}
func SessionHeaders(presetID, id string) map[string]string {
	if strings.HasPrefix(presetID, "opencode") {
		return map[string]string{"X-Opencode-Session": id}
	}
	if presetID == "github-copilot" {
		return map[string]string{"X-Interaction-Id": id}
	}
	return nil
}
