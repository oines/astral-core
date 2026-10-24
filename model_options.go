package astral

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
)

// GenerationOptions is the caller's frozen selection for one model call.
// A nil pointer means "use the provider default"; explicit zero and false
// remain distinct. Providers validate their own supported values and
// combinations. Core never translates reasoning effort names.
type GenerationOptions struct {
	Temperature      *float64                   `json:"temperature,omitempty"`
	TopP             *float64                   `json:"top_p,omitempty"`
	Stop             []string                   `json:"stop,omitempty"`
	Seed             *int64                     `json:"seed,omitempty"`
	FrequencyPenalty *float64                   `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64                   `json:"presence_penalty,omitempty"`
	Reasoning        ReasoningOptions           `json:"reasoning,omitempty"`
	ToolChoice       ToolChoice                 `json:"tool_choice,omitempty"`
	ResponseFormat   *ResponseFormat            `json:"response_format,omitempty"`
	ProviderOptions  map[string]json.RawMessage `json:"provider_options,omitempty"`
}

type ReasoningOptions struct {
	Enabled      *bool  `json:"enabled,omitempty"`
	Effort       string `json:"effort,omitempty"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
}

type ToolChoice struct {
	// Mode is empty (provider default), auto, none, required, or tool.
	Mode string `json:"mode,omitempty"`
	Name string `json:"name,omitempty"`
}

type ResponseFormat struct {
	// Type is text, json_object, or json_schema.
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
}

// Validate checks only protocol-neutral shape and numeric validity.
// Provider capabilities and dialect-specific constraints belong to adapters.
func (o GenerationOptions) Validate() error {
	for name, value := range map[string]*float64{
		"temperature": o.Temperature, "top_p": o.TopP,
		"frequency_penalty": o.FrequencyPenalty, "presence_penalty": o.PresencePenalty,
	} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return fmtInvalid("%s must be finite", name)
		}
	}
	if o.Temperature != nil && *o.Temperature < 0 {
		return fmtInvalid("temperature must not be negative")
	}
	if o.TopP != nil && (*o.TopP < 0 || *o.TopP > 1) {
		return fmtInvalid("top_p must be between 0 and 1")
	}
	if o.Reasoning.BudgetTokens != nil && *o.Reasoning.BudgetTokens < 0 {
		return fmtInvalid("reasoning budget must not be negative")
	}
	if strings.TrimSpace(o.Reasoning.Effort) != o.Reasoning.Effort {
		return fmtInvalid("reasoning effort must not have surrounding whitespace")
	}
	switch o.ToolChoice.Mode {
	case "", "auto", "none", "required":
		if o.ToolChoice.Name != "" {
			return fmtInvalid("a named tool choice requires mode tool")
		}
	case "tool":
		if strings.TrimSpace(o.ToolChoice.Name) == "" {
			return fmtInvalid("tool choice requires a tool name")
		}
	default:
		return fmtInvalid("unknown tool choice mode %q", o.ToolChoice.Mode)
	}
	if f := o.ResponseFormat; f != nil {
		switch f.Type {
		case "text", "json_object":
			if len(f.Schema) != 0 || f.Name != "" || f.Strict != nil {
				return fmtInvalid("only json_schema accepts a name, schema, or strict flag")
			}
		case "json_schema":
			if strings.TrimSpace(f.Name) == "" || !jsonObject(f.Schema) {
				return fmtInvalid("json_schema requires a name and an object schema")
			}
		default:
			return fmtInvalid("unknown response format %q", f.Type)
		}
	}
	for namespace, raw := range o.ProviderOptions {
		if strings.TrimSpace(namespace) == "" || !jsonObject(raw) {
			return fmtInvalid("provider options require a namespace and an object")
		}
	}
	return nil
}

func jsonObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{' && json.Valid(raw)
}
