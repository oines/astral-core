package models

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	core "github.com/oines/astral-core"
)

// ReasoningFormat names a request encoding, not a model family. The host binds
// it to one connection/protocol/model route; model names never select it here.
type ReasoningFormat string

const (
	ReasoningFormatProtocolDefault   ReasoningFormat = "protocol-default"
	ReasoningFormatOpenAIChat        ReasoningFormat = "openai-chat"
	ReasoningFormatOpenAIResponses   ReasoningFormat = "openai-responses"
	ReasoningFormatThinkingType      ReasoningFormat = "thinking-type"
	ReasoningFormatThinkingBoolean   ReasoningFormat = "thinking-boolean"
	ReasoningFormatQwen              ReasoningFormat = "qwen"
	ReasoningFormatChatTemplate      ReasoningFormat = "chat-template"
	ReasoningFormatOpenRouter        ReasoningFormat = "openrouter"
	ReasoningFormatTogether          ReasoningFormat = "together"
	ReasoningFormatFireworks         ReasoningFormat = "fireworks"
	ReasoningFormatAnthropic         ReasoningFormat = "anthropic"
	ReasoningFormatAnthropicBudget   ReasoningFormat = "anthropic-budget"
	ReasoningFormatAnthropicAdaptive ReasoningFormat = "anthropic-adaptive"
	ReasoningFormatGemini            ReasoningFormat = "gemini"
	ReasoningFormatGeminiBudget      ReasoningFormat = "gemini-budget"
	ReasoningFormatGeminiLevel       ReasoningFormat = "gemini-level"
	ReasoningFormatCustom            ReasoningFormat = "custom"
)

// ReasoningOption contains the exact semantic selection the host may persist.
// Hosts add their own inheritance option; interface-default is an explicit
// empty selection that bypasses profile defaults before reaching this SDK.
type ReasoningOption struct {
	ID    string                `json:"id"`
	Value core.ReasoningOptions `json:"value"`
}

type ReasoningBudgetControl struct {
	Min *int `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
}

type ReasoningDiagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

type ReasoningControls struct {
	Format      ReasoningFormat         `json:"format"`
	Options     []ReasoningOption       `json:"options"`
	Budget      *ReasoningBudgetControl `json:"budget,omitempty"`
	Diagnostics []ReasoningDiagnostic   `json:"diagnostics,omitempty"`
}

// ReasoningEncoding is credential-free. Fields contains only the selected
// reasoning parameters; unrelated fields sharing their parent objects survive.
type ReasoningEncoding struct {
	Fields      map[string]any        `json:"fields"`
	OwnedFields []string              `json:"owned_fields"`
	Selection   core.ReasoningOptions `json:"selection"`
	Diagnostics []ReasoningDiagnostic `json:"diagnostics,omitempty"`
}

// MarshalJSON preserves the difference between an unknown effort list and an
// explicitly empty one when recommendations from several sources are merged.
func (c ReasoningCapabilities) MarshalJSON() ([]byte, error) {
	type wire ReasoningCapabilities
	raw, err := json.Marshal(wire(c))
	if err != nil || c.Efforts != nil {
		return raw, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "efforts")
	return json.Marshal(fields)
}

type reasoningPlan struct {
	format                ReasoningFormat
	caps                  *ReasoningCapabilities
	mapping               *compiledReasoningMapping
	maxOutput             int
	requireAdaptiveEffort bool
}

func reasoningError(path, message string) error {
	return fmt.Errorf("%w: reasoning.%s: %s", core.ErrInvalid, path, message)
}

func boolValue(v *bool) bool     { return v != nil && *v }
func reasoningBool(v bool) *bool { return &v }
func reasoningInt(v int) *int    { return &v }

func reasoningBudgetSupported(c *ReasoningCapabilities) bool {
	if c == nil {
		return false
	}
	if c.Budget != nil {
		return *c.Budget
	}
	return c.MinBudget != nil || c.MaxBudget != nil || boolValue(c.DynamicBudget)
}

func resolveReasoningPlan(protocol string, config Config) (reasoningPlan, error) {
	p := reasoningPlan{format: config.ReasoningFormat, caps: config.Reasoning, maxOutput: config.Spec.MaxOutputTokens, requireAdaptiveEffort: config.RequireAdaptiveEffort}
	if p.format == "" || p.format == ReasoningFormatProtocolDefault {
		switch protocol {
		case "openai-completions":
			p.format = ReasoningFormatOpenAIChat
		case "openai-responses":
			p.format = ReasoningFormatOpenAIResponses
		case "anthropic-messages":
			p.format = ReasoningFormatAnthropic
		case "google-generative-ai":
			p.format = ReasoningFormatGemini
		default:
			return p, reasoningError("format", "unsupported protocol "+protocol)
		}
	}
	valid := false
	switch p.format {
	case ReasoningFormatOpenAIChat, ReasoningFormatThinkingType, ReasoningFormatThinkingBoolean,
		ReasoningFormatQwen, ReasoningFormatChatTemplate, ReasoningFormatOpenRouter, ReasoningFormatTogether, ReasoningFormatFireworks:
		valid = protocol == "openai-completions"
	case ReasoningFormatOpenAIResponses:
		valid = protocol == "openai-responses"
	case ReasoningFormatAnthropic, ReasoningFormatAnthropicBudget, ReasoningFormatAnthropicAdaptive:
		valid = protocol == "anthropic-messages"
	case ReasoningFormatGemini, ReasoningFormatGeminiBudget, ReasoningFormatGeminiLevel:
		valid = protocol == "google-generative-ai"
	case ReasoningFormatCustom:
		valid = slices.Contains([]string{"openai-completions", "openai-responses", "anthropic-messages", "google-generative-ai"}, protocol)
	default:
		return p, reasoningError("format", "unknown format "+string(p.format))
	}
	if !valid {
		return p, reasoningError("format", fmt.Sprintf("format %q cannot be used with protocol %q", p.format, protocol))
	}
	if p.format == ReasoningFormatCustom {
		var err error
		p.mapping, err = compileReasoningMapping(config.ReasoningMapping)
		if err != nil {
			return p, err
		}
	} else if config.ReasoningMapping != nil {
		return p, reasoningError("mapping", "a custom mapping requires the custom format")
	}
	if c := p.caps; c != nil {
		if c.MinBudget != nil && *c.MinBudget < 0 || c.MaxBudget != nil && *c.MaxBudget < 0 {
			return p, reasoningError("budget", "bounds must not be negative")
		}
		if c.MinBudget != nil && c.MaxBudget != nil && *c.MinBudget > *c.MaxBudget {
			return p, reasoningError("budget", "minimum exceeds maximum")
		}
		for _, value := range c.Efforts {
			if value == "" || strings.TrimSpace(value) != value {
				return p, reasoningError("efforts", "values must be nonempty and have no surrounding whitespace")
			}
		}
	}
	return p, nil
}

func (p reasoningPlan) effortValues() []string {
	var values []string
	if p.caps != nil && p.caps.Efforts != nil {
		values = p.caps.Efforts
	} else if p.mapping != nil {
		for value := range p.mapping.efforts {
			values = append(values, value)
		}
		sort.Strings(values)
	}
	var out []string
	for _, value := range values {
		if p.caps != nil && value == p.caps.OffEffort {
			continue
		}
		if !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func (p reasoningPlan) budgetBounds() *ReasoningBudgetControl {
	if !reasoningBudgetSupported(p.caps) {
		return nil
	}
	switch p.format {
	case ReasoningFormatQwen, ReasoningFormatChatTemplate, ReasoningFormatOpenRouter,
		ReasoningFormatFireworks, ReasoningFormatAnthropic, ReasoningFormatAnthropicBudget, ReasoningFormatGemini, ReasoningFormatGeminiBudget:
	default:
		return nil
	}
	b := &ReasoningBudgetControl{Min: p.caps.MinBudget, Max: p.caps.MaxBudget}
	if p.format == ReasoningFormatAnthropic || p.format == ReasoningFormatAnthropicBudget || p.format == ReasoningFormatFireworks {
		minimum := 1024
		if b.Min == nil || *b.Min < minimum {
			b.Min = &minimum
		}
		if p.maxOutput > 0 && (b.Max == nil || *b.Max >= p.maxOutput) {
			b.Max = reasoningInt(p.maxOutput - 1)
		}
	}
	if b.Min != nil && b.Max != nil && *b.Min > *b.Max {
		return nil
	}
	return b
}

func (p reasoningPlan) checkCapabilities(o core.ReasoningOptions) error {
	if o.Enabled == nil && o.Effort == "" && o.BudgetTokens == nil {
		return nil
	}
	c := p.caps
	if c != nil && c.Supported != nil && !*c.Supported {
		return reasoningError("supported", "this route does not support reasoning")
	}
	if o.Enabled != nil {
		if *o.Enabled && c != nil && c.Toggle != nil && !*c.Toggle {
			return reasoningError("enabled", "this route has no independent reasoning switch")
		}
		if !*o.Enabled && c != nil && c.Disable != nil && !*c.Disable {
			return reasoningError("enabled", "reasoning cannot be disabled on this route")
		}
		if *o.Enabled && (c == nil || c.Toggle == nil) && p.mapping == nil {
			return reasoningError("enabled", "no reasoning switch is declared")
		}
		if !*o.Enabled && (c == nil || c.Disable == nil && c.OffEffort == "") && p.mapping == nil {
			return reasoningError("enabled", "no reasoning-off control is declared")
		}
	}
	if o.Effort != "" {
		off := c != nil && c.OffEffort != "" && o.Effort == c.OffEffort
		if off && c.Disable != nil && !*c.Disable {
			return reasoningError("effort", "the declared off value is unavailable on this route")
		}
		if !off && c != nil && c.Efforts != nil && !slices.Contains(c.Efforts, o.Effort) {
			return reasoningError("effort", fmt.Sprintf("unsupported value %q", o.Effort))
		}
		if !off && (c == nil || c.Efforts == nil) && p.mapping == nil {
			return reasoningError("effort", "no accepted effort values are declared")
		}
	}
	if o.BudgetTokens != nil {
		if *o.BudgetTokens < 0 {
			return reasoningError("budget_tokens", "budget must not be negative")
		}
		if c != nil {
			if c.Budget != nil && !*c.Budget {
				return reasoningError("budget_tokens", "this route has no budget control")
			}
			if c.MinBudget != nil && *o.BudgetTokens < *c.MinBudget || c.MaxBudget != nil && *o.BudgetTokens > *c.MaxBudget {
				return reasoningError("budget_tokens", "budget is outside the declared range")
			}
		}
		if !reasoningBudgetSupported(c) {
			return reasoningError("budget_tokens", "no budget control is declared")
		}
	}
	return nil
}

// ResolveReasoningControls returns only choices that the exact same encoder can
// represent. The host resolves recommendation precedence before calling it.
func ResolveReasoningControls(protocol string, config Config) (ReasoningControls, error) {
	p, err := resolveReasoningPlan(protocol, config)
	if err != nil {
		return ReasoningControls{}, err
	}
	out := ReasoningControls{Format: p.format, Options: []ReasoningOption{{ID: "interface-default", Value: core.ReasoningOptions{}}}}
	if p.caps != nil && p.caps.Supported != nil && !*p.caps.Supported {
		return out, nil
	}
	appendOption := func(id string, value core.ReasoningOptions) {
		if _, err := p.encode(value, p.maxOutput); err == nil {
			out.Options = append(out.Options, ReasoningOption{ID: id, Value: value})
		} else {
			out.Diagnostics = append(out.Diagnostics, ReasoningDiagnostic{Code: "unavailable-option", Path: "reasoning.options." + id, Message: err.Error()})
		}
	}
	if p.mapping != nil && p.mapping.off != nil || p.caps != nil && (boolValue(p.caps.Disable) || p.caps.OffEffort != "") {
		appendOption("off", core.ReasoningOptions{Enabled: reasoningBool(false)})
	}
	if p.mapping != nil && p.mapping.on != nil || p.caps != nil && boolValue(p.caps.Toggle) {
		appendOption("on", core.ReasoningOptions{Enabled: reasoningBool(true)})
	}
	for _, value := range p.effortValues() {
		option := core.ReasoningOptions{Effort: value}
		if p.caps != nil && boolValue(p.caps.Toggle) {
			option.Enabled = reasoningBool(true)
		}
		appendOption("effort:"+value, option)
	}
	out.Budget = p.budgetBounds()
	if reasoningBudgetSupported(p.caps) && out.Budget == nil {
		out.Diagnostics = append(out.Diagnostics, ReasoningDiagnostic{Code: "unavailable-budget", Path: "reasoning.budget", Message: "the selected format or output limit cannot express the declared budget control"})
	}
	return out, nil
}

// EncodeReasoning does not mutate config or options. It validates the selection
// and emits one format without falling back to another value or field layout.
func EncodeReasoning(protocol string, config Config, options core.ReasoningOptions, maxOutputTokens int) (ReasoningEncoding, error) {
	p, err := resolveReasoningPlan(protocol, config)
	if err != nil {
		return ReasoningEncoding{}, err
	}
	fields, err := p.encode(options, maxOutputTokens)
	if err != nil {
		return ReasoningEncoding{}, err
	}
	return ReasoningEncoding{Fields: fields, OwnedFields: p.ownedFields(), Selection: options}, nil
}

func (p reasoningPlan) encode(o core.ReasoningOptions, maxOutput int) (map[string]any, error) {
	if err := p.checkCapabilities(o); err != nil {
		return nil, err
	}
	body := map[string]any{}
	if o.Enabled == nil && o.Effort == "" && o.BudgetTokens == nil {
		return body, nil
	}
	if o.BudgetTokens != nil && o.Effort != "" {
		return nil, reasoningError("budget_tokens", "budget and effort cannot be selected together")
	}
	offValue := ""
	if p.caps != nil {
		offValue = p.caps.OffEffort
	}
	disabled := o.Enabled != nil && !*o.Enabled || offValue != "" && o.Effort == offValue
	if o.Enabled != nil && *o.Enabled && offValue != "" && o.Effort == offValue {
		return nil, reasoningError("effort", "the off value conflicts with enabling reasoning")
	}
	if disabled && (o.Effort != "" && o.Effort != offValue || o.BudgetTokens != nil && *o.BudgetTokens != 0) {
		return nil, reasoningError("enabled", "disabling reasoning conflicts with effort or a nonzero budget")
	}
	if p.mapping != nil {
		return p.mapping.encode(o, disabled)
	}
	effort := o.Effort
	if o.Enabled != nil && *o.Enabled && effort == "" && o.BudgetTokens == nil && p.caps != nil && p.caps.OnEffort != "" {
		effort = p.caps.OnEffort
	}
	switch p.format {
	case ReasoningFormatOpenAIChat, ReasoningFormatOpenAIResponses:
		if o.BudgetTokens != nil {
			return nil, reasoningError("budget_tokens", "this format does not encode a token budget")
		}
		if disabled {
			if offValue == "" {
				return nil, reasoningError("enabled", "this route has no declared reasoning-off value")
			}
			effort = offValue
		}
		if o.Enabled != nil && *o.Enabled && effort == "" {
			return nil, reasoningError("enabled", "enabling reasoning requires a declared active effort")
		}
		if effort != "" {
			if p.format == ReasoningFormatOpenAIChat {
				body["reasoning_effort"] = effort
			} else {
				body["reasoning"] = map[string]any{"effort": effort}
			}
		}
	case ReasoningFormatThinkingType, ReasoningFormatThinkingBoolean, ReasoningFormatTogether:
		if o.BudgetTokens != nil {
			return nil, reasoningError("budget_tokens", "this format does not encode a token budget")
		}
		if o.Enabled != nil || disabled {
			switch p.format {
			case ReasoningFormatThinkingType:
				value := "enabled"
				if disabled {
					value = "disabled"
				}
				body["thinking"] = map[string]any{"type": value}
			case ReasoningFormatThinkingBoolean:
				body["thinking"] = !disabled
			case ReasoningFormatTogether:
				body["reasoning"] = map[string]any{"enabled": !disabled}
			}
		}
		if effort != "" && !disabled {
			body["reasoning_effort"] = effort
		}
	case ReasoningFormatQwen, ReasoningFormatChatTemplate:
		fields := body
		if p.format == ReasoningFormatChatTemplate {
			fields = map[string]any{}
			body["chat_template_kwargs"] = fields
		}
		if o.Enabled != nil || disabled {
			fields["enable_thinking"] = !disabled
		}
		if o.BudgetTokens != nil {
			fields["thinking_budget"] = *o.BudgetTokens
		}
		if effort != "" && !disabled {
			body["reasoning_effort"] = effort
		}
	case ReasoningFormatOpenRouter:
		fields := map[string]any{}
		if o.Enabled != nil || disabled {
			fields["enabled"] = !disabled
		}
		if !disabled && effort != "" {
			fields["effort"] = effort
		}
		if !disabled && o.BudgetTokens != nil {
			fields["max_tokens"] = *o.BudgetTokens
		}
		body["reasoning"] = fields
	case ReasoningFormatFireworks, ReasoningFormatAnthropic, ReasoningFormatAnthropicBudget, ReasoningFormatAnthropicAdaptive:
		if disabled {
			body["thinking"] = map[string]any{"type": "disabled"}
			break
		}
		if o.BudgetTokens != nil {
			if p.format == ReasoningFormatAnthropicAdaptive {
				return nil, reasoningError("budget_tokens", "adaptive thinking does not accept a token budget")
			}
			if *o.BudgetTokens < 1024 || maxOutput > 0 && *o.BudgetTokens >= maxOutput {
				return nil, reasoningError("budget_tokens", "manual thinking requires a budget of at least 1024 and below max output tokens")
			}
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": *o.BudgetTokens}
		} else if p.format == ReasoningFormatFireworks {
			if effort == "" {
				return nil, reasoningError("enabled", "enabling this format requires an effort or budget")
			}
			body["reasoning_effort"] = effort
		} else {
			if p.format == ReasoningFormatAnthropicBudget {
				return nil, reasoningError("budget_tokens", "enabling manual thinking requires an explicit token budget")
			}
			if p.format == ReasoningFormatAnthropic && (p.caps == nil || len(p.caps.Efforts) == 0) {
				return nil, reasoningError("format", "select a manual budget or declare adaptive thinking support")
			}
			if p.requireAdaptiveEffort && effort == "" {
				return nil, reasoningError("effort", "this API requires an explicit effort with adaptive thinking; enabling requires a declared active effort")
			}
			body["thinking"] = map[string]any{"type": "adaptive"}
			if effort != "" {
				body["output_config"] = map[string]any{"effort": effort}
			}
		}
	case ReasoningFormatGemini, ReasoningFormatGeminiBudget, ReasoningFormatGeminiLevel:
		fields := map[string]any{}
		if disabled {
			if p.format == ReasoningFormatGeminiLevel {
				return nil, reasoningError("enabled", "thinking levels do not provide a reasoning-off control")
			}
			fields["thinkingBudget"] = 0
		} else if o.BudgetTokens != nil {
			if p.format == ReasoningFormatGeminiLevel {
				return nil, reasoningError("budget_tokens", "this model uses thinking levels")
			}
			fields["thinkingBudget"] = *o.BudgetTokens
		} else if effort != "" {
			if p.format == ReasoningFormatGeminiBudget {
				return nil, reasoningError("effort", "this model uses a thinking budget")
			}
			fields["thinkingLevel"] = effort
		} else if p.caps != nil && boolValue(p.caps.DynamicBudget) && p.format != ReasoningFormatGeminiLevel {
			fields["thinkingBudget"] = -1
		} else {
			return nil, reasoningError("enabled", "enabling reasoning requires a declared level or budget")
		}
		body["generationConfig"] = map[string]any{"thinkingConfig": fields}
	}
	return body, nil
}

func (p reasoningPlan) ownedFields() []string {
	if p.mapping != nil {
		return slices.Clone(p.mapping.fields)
	}
	switch p.format {
	case ReasoningFormatOpenAIChat:
		return []string{"reasoning_effort"}
	case ReasoningFormatOpenAIResponses:
		return []string{"reasoning.effort"}
	case ReasoningFormatThinkingType:
		return []string{"thinking.type", "reasoning_effort"}
	case ReasoningFormatThinkingBoolean:
		return []string{"thinking", "reasoning_effort"}
	case ReasoningFormatTogether:
		return []string{"reasoning.enabled", "reasoning_effort"}
	case ReasoningFormatQwen:
		return []string{"enable_thinking", "thinking_budget", "reasoning_effort"}
	case ReasoningFormatChatTemplate:
		return []string{"chat_template_kwargs.enable_thinking", "chat_template_kwargs.thinking_budget", "reasoning_effort"}
	case ReasoningFormatOpenRouter:
		return []string{"reasoning.enabled", "reasoning.effort", "reasoning.max_tokens"}
	case ReasoningFormatFireworks:
		return []string{"thinking.type", "thinking.budget_tokens", "reasoning_effort"}
	case ReasoningFormatAnthropic, ReasoningFormatAnthropicBudget, ReasoningFormatAnthropicAdaptive:
		return []string{"thinking.type", "thinking.budget_tokens", "output_config.effort"}
	case ReasoningFormatGemini, ReasoningFormatGeminiBudget, ReasoningFormatGeminiLevel:
		return []string{"generationConfig.thinkingConfig.thinkingBudget", "generationConfig.thinkingConfig.thinkingLevel"}
	}
	return nil
}
