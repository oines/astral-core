package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	core "github.com/oines/astral-core"
)

// ReasoningMapping contains finite, literal request fragments. An effort
// fragment is complete: it does not implicitly merge the on fragment.
type ReasoningMapping struct {
	Fields  []string                   `json:"fields"`
	On      json.RawMessage            `json:"on,omitempty"`
	Off     json.RawMessage            `json:"off,omitempty"`
	Efforts map[string]json.RawMessage `json:"efforts,omitempty"`
}

type compiledReasoningMapping struct {
	fields  []string
	on, off map[string]any
	efforts map[string]map[string]any
}

func compileReasoningMapping(mapping *ReasoningMapping) (*compiledReasoningMapping, error) {
	if mapping == nil {
		return nil, reasoningError("mapping", "custom format requires a mapping")
	}
	if len(mapping.Efforts) > 64 || len(mapping.Fields) > 64 {
		return nil, reasoningError("mapping", "at most 64 fields and efforts are allowed")
	}
	result := &compiledReasoningMapping{fields: slices.Clone(mapping.Fields), efforts: map[string]map[string]any{}}
	var leaves []string
	parse := func(label string, raw json.RawMessage) (map[string]any, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		if len(raw) > 65536 {
			return nil, reasoningError("mapping."+label, "fragment exceeds 64 KiB")
		}
		var value map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if !json.Valid(raw) || decoder.Decode(&value) != nil || len(value) == 0 {
			return nil, reasoningError("mapping."+label, "expected a nonempty JSON object")
		}
		paths, err := reasoningLeafPaths(value, "", 0)
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			if err := validateReasoningField(path); err != nil {
				return nil, err
			}
			if !slices.Contains(leaves, path) {
				leaves = append(leaves, path)
			}
		}
		return value, nil
	}
	var err error
	if result.on, err = parse("on", mapping.On); err != nil {
		return nil, err
	}
	if result.off, err = parse("off", mapping.Off); err != nil {
		return nil, err
	}
	for effort, raw := range mapping.Efforts {
		if effort == "" || strings.TrimSpace(effort) != effort {
			return nil, reasoningError("mapping.efforts", "effort keys must be nonempty and have no surrounding whitespace")
		}
		value, err := parse("efforts."+effort, raw)
		if err != nil {
			return nil, err
		}
		if value == nil {
			return nil, reasoningError("mapping.efforts."+effort, "an effort needs a nonempty fragment")
		}
		result.efforts[effort] = value
	}
	if len(leaves) == 0 || len(leaves) > 64 {
		return nil, reasoningError("mapping", "mapping must own between 1 and 64 fields")
	}
	if len(result.fields) == 0 {
		result.fields = slices.Clone(leaves)
	}
	for _, path := range result.fields {
		if err := validateReasoningField(path); err != nil {
			return nil, err
		}
		if !slices.Contains(leaves, path) {
			return nil, reasoningError("mapping.fields", fmt.Sprintf("field %q is not produced by a fragment", path))
		}
	}
	for _, path := range leaves {
		if !slices.Contains(result.fields, path) {
			return nil, reasoningError("mapping.fields", fmt.Sprintf("fragment field %q is not registered", path))
		}
		for _, other := range leaves {
			if path != other && strings.HasPrefix(path, other+".") {
				return nil, reasoningError("mapping.fields", fmt.Sprintf("field %q changes between an object and a scalar", other))
			}
		}
	}
	sort.Strings(result.fields)
	result.fields = slices.Compact(result.fields)
	return result, nil
}

func reasoningLeafPaths(value map[string]any, prefix string, depth int) ([]string, error) {
	if depth > 8 {
		return nil, reasoningError("mapping", "object nesting exceeds 8 levels")
	}
	var paths []string
	for key, child := range value {
		if key == "" || strings.Contains(key, ".") || strings.TrimSpace(key) != key {
			return nil, reasoningError("mapping", "field names must be nonempty, without dots or surrounding whitespace")
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		switch nested := child.(type) {
		case map[string]any:
			if len(nested) == 0 {
				return nil, reasoningError("mapping."+path, "empty objects do not express a reasoning control")
			}
			children, err := reasoningLeafPaths(nested, path, depth+1)
			if err != nil {
				return nil, err
			}
			paths = append(paths, children...)
		case []any:
			return nil, reasoningError("mapping."+path, "reasoning fragments accept scalar values and objects, not arrays")
		default:
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func validateReasoningField(path string) error {
	parts := strings.Split(path, ".")
	for _, part := range parts {
		if part == "" || strings.TrimSpace(part) != part {
			return reasoningError("mapping.fields", "invalid field path "+path)
		}
	}
	root := parts[0]
	switch root {
	case "model", "messages", "input", "instructions", "system", "contents", "systemInstruction", "tools", "tool_choice", "toolConfig", "functions", "function_call",
		"stream", "stream_options", "max_tokens", "max_completion_tokens", "max_output_tokens", "previous_response_id", "conversation", "background", "truncation",
		"temperature", "top_p", "top_k", "seed", "stop", "stop_sequences", "n", "frequency_penalty", "presence_penalty", "response_format", "text",
		"headers", "authorization", "api_key", "apiKey", "key", "base_url", "endpoint", "url", "user", "metadata", "store", "service_tier", "safety_settings", "safetySettings":
		return reasoningError("mapping.fields", fmt.Sprintf("field %q belongs to the request or connection contract", path))
	case "generationConfig":
		if !strings.HasPrefix(path, "generationConfig.thinkingConfig.") {
			return reasoningError("mapping.fields", "only generationConfig.thinkingConfig fields may be managed")
		}
	case "output_config":
		if path != "output_config.effort" {
			return reasoningError("mapping.fields", "only output_config.effort may be managed")
		}
	}
	return nil
}

func (m *compiledReasoningMapping) encode(o core.ReasoningOptions, disabled bool) (map[string]any, error) {
	if o.BudgetTokens != nil {
		return nil, reasoningError("budget_tokens", "custom fragments do not support dynamic budget binding")
	}
	var source map[string]any
	switch {
	case disabled:
		source = m.off
	case o.Effort != "":
		source = m.efforts[o.Effort]
	case o.Enabled != nil && *o.Enabled:
		source = m.on
	}
	if source == nil {
		return nil, reasoningError("mapping", "the mapping does not express this selection")
	}
	raw, _ := json.Marshal(source)
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

// ReasoningFormats is the authoritative format inventory for the host UI.
// Empty protocol lists every format. Compatibility never changes the protocol.
func ReasoningFormats(protocol string) []ReasoningFormat {
	all := []ReasoningFormat{ReasoningFormatProtocolDefault, ReasoningFormatOpenAIChat, ReasoningFormatOpenAIResponses, ReasoningFormatThinkingType,
		ReasoningFormatThinkingBoolean, ReasoningFormatQwen, ReasoningFormatChatTemplate, ReasoningFormatOpenRouter, ReasoningFormatTogether,
		ReasoningFormatFireworks, ReasoningFormatAnthropic, ReasoningFormatAnthropicBudget, ReasoningFormatAnthropicAdaptive,
		ReasoningFormatGemini, ReasoningFormatGeminiBudget, ReasoningFormatGeminiLevel, ReasoningFormatCustom}
	if protocol == "" {
		return all
	}
	var out []ReasoningFormat
	for _, format := range all {
		if format == ReasoningFormatCustom {
			if slices.Contains([]string{"openai-completions", "openai-responses", "anthropic-messages", "google-generative-ai"}, protocol) {
				out = append(out, format)
			}
			continue
		}
		if _, err := resolveReasoningPlan(protocol, Config{ReasoningFormat: format}); err == nil {
			out = append(out, format)
		}
	}
	return out
}

// ValidateReasoningExtensions prevents unmodeled request extras from overriding
// a semantic selection, including interface-default. Non-control siblings such
// as reasoning.summary remain available. Legacy unbound callers retain their
// existing extension behavior until a format is explicitly configured.
func ValidateReasoningExtensions(protocol string, config Config, o core.ReasoningOptions, body, extensions map[string]any) error {
	if config.ReasoningFormat == "" {
		return nil
	}
	p, err := resolveReasoningPlan(protocol, config)
	if err != nil {
		return err
	}
	owned := p.ownedFields()
	owned = append(owned, "reasoning_effort", "reasoning.effort", "reasoning.enabled", "reasoning.max_tokens", "thinking.type", "thinking.budget_tokens",
		"enable_thinking", "thinking_budget", "chat_template_kwargs.enable_thinking", "chat_template_kwargs.thinking_budget",
		"output_config.effort", "generationConfig.thinkingConfig.thinkingBudget", "generationConfig.thinkingConfig.thinkingLevel")
	for _, path := range owned {
		// Reject a scalar/null parent before checking the leaf. Otherwise
		// reasoning:null could erase an encoded effort during deep merge.
		parent := extensions
		parts := strings.Split(path, ".")
		for i, part := range parts[:len(parts)-1] {
			value, exists := parent[part]
			if !exists {
				break
			}
			nested, object := value.(map[string]any)
			if !object {
				return reasoningError("provider_options."+strings.Join(parts[:i+1], "."), "extension replaces an object containing managed reasoning fields")
			}
			parent = nested
		}
		value, exists := reasoningPath(extensions, path)
		if !exists {
			continue
		}
		selected, generated := reasoningPath(body, path)
		left, _ := json.Marshal(value)
		right, _ := json.Marshal(selected)
		if !generated || !bytes.Equal(left, right) {
			return reasoningError("provider_options."+path, "extension conflicts with the resolved reasoning selection; use a reasoning mapping instead")
		}
	}
	// A scalar parent (for example thinking:false) must not bypass leaf checks.
	if value, exists := extensions["thinking"]; exists {
		if _, object := value.(map[string]any); !object {
			selected, generated := body["thinking"]
			left, _ := json.Marshal(value)
			right, _ := json.Marshal(selected)
			if !generated || !bytes.Equal(left, right) {
				return reasoningError("provider_options.thinking", "extension conflicts with the resolved reasoning selection")
			}
		}
	}
	return nil
}

func reasoningPath(object map[string]any, path string) (any, bool) {
	var value any = object
	for _, part := range strings.Split(path, ".") {
		fields, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok = fields[part]
		if !ok {
			return nil, false
		}
	}
	return value, true
}
