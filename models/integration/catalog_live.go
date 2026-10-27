package integration

import (
	"encoding/json"
	"math"
	"strconv"

	models "github.com/oines/astral-core/models"
)

// liveModelRecommendations deliberately parses sparse raw fields. A nullable
// capability or explicit empty list must not become indistinguishable from an
// omitted field when public recommendations are merged below it.
func liveModelRecommendations(provider ProviderConfig, item json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item, &fields); err != nil {
		return nil, err
	}
	r := map[string]json.RawMessage{}
	paths := map[string][][]string{
		"context_window":    {{"capabilities", "limits", "max_context_window_tokens"}, {"context_window"}, {"context_length"}, {"max_context_window"}},
		"max_input_tokens":  {{"inputTokenLimit"}, {"capabilities", "limits", "max_prompt_tokens"}},
		"max_output_tokens": {{"outputTokenLimit"}, {"capabilities", "limits", "max_output_tokens"}, {"max_output_tokens"}, {"top_provider", "max_completion_tokens"}},
	}
	if provider.PresetID == "floway" {
		paths["context_window"] = append([][]string{{"limits", "max_context_window_tokens"}}, paths["context_window"]...)
		paths["max_input_tokens"] = append([][]string{{"limits", "max_prompt_tokens"}}, paths["max_input_tokens"]...)
		paths["max_output_tokens"] = append([][]string{{"limits", "max_output_tokens"}}, paths["max_output_tokens"]...)
	}
	if provider.PresetID == "magpie" || provider.PresetID == "litellm" {
		paths["max_input_tokens"] = append([][]string{{"max_input_tokens"}}, paths["max_input_tokens"]...)
	}
	if provider.PresetID == "litellm" {
		// LiteLLM defines max_input_tokens as its prompt/context ceiling.
		// Its legacy max_tokens field may instead mean output; never use it.
		paths["context_window"] = append(paths["context_window"], []string{"max_input_tokens"})
	}
	for name, alternatives := range paths {
		if value, ok := firstJSONField(fields, alternatives...); ok {
			var number int
			if isNull(value) || valueFromJSON(value, &number) && number >= 0 {
				r[name] = value
			}
		}
	}
	if provider.Protocol == "google-generative-ai" {
		input, hasInput := positiveInteger(r["max_input_tokens"])
		output, hasOutput := positiveInteger(r["max_output_tokens"])
		if hasInput && hasOutput {
			r["context_window"] = raw(input + output)
		}
	}
	for name, alternatives := range map[string][][]string{
		"tool_calling": {{"capabilities", "supports", "tool_calls"}, {"tool_call"}},
		"images":       {{"capabilities", "supports", "vision"}},
	} {
		if value, present := firstJSONField(fields, alternatives...); present {
			if isNull(value) {
				r[name] = value
			} else if yes, ok := explicitBool(value); ok {
				r[name] = capabilityStatus(yes)
			}
		}
	}
	modalitiesPaths := [][]string{{"input_modalities"}, {"architecture", "input_modalities"}}
	switch provider.PresetID {
	case "floway":
		modalitiesPaths = append([][]string{{"chat", "modalities", "input"}}, modalitiesPaths...)
	case "magpie":
		modalitiesPaths = append([][]string{{"modalities", "input"}}, modalitiesPaths...)
	}
	if inputs, present := firstJSONField(fields, modalitiesPaths...); present {
		if isNull(inputs) {
			r["images"] = inputs
		} else if modalities, ok := stringList(inputs); ok {
			r["images"] = capabilityStatus(containsString(modalities, "image"))
		}
	}
	if provider.PresetID == "litellm" {
		for from, to := range map[string]string{"supports_function_calling": "tool_calling", "supports_vision": "images"} {
			if value, present := fields[from]; present {
				if isNull(value) {
					r[to] = value
				} else if yes, ok := explicitBool(value); ok {
					r[to] = capabilityStatus(yes)
				}
			}
		}
	}
	for from, to := range map[string]string{"prompt": "input_price", "completion": "output_price"} {
		if value, present := firstJSONField(fields, []string{"pricing", from}); present {
			if price, valid := perMillionTokenPrice(value); valid {
				r[to] = price
			}
		}
	}
	if provider.PresetID == "floway" {
		for from, value := range flowayBaseRates(fields) {
			to := map[string]string{"input_tokens": "input_price", "output_tokens": "output_price"}[from]
			if price, valid := perMillionTokenPrice(value); valid && to != "" {
				r[to] = price
			}
		}
	}
	source := provider.PresetID
	if source == "" {
		source = provider.Dialect
	}
	caps, err := models.ParseReasoningMetadata(source, item)
	if err != nil {
		return nil, err
	}
	if len(caps) > 0 {
		r["reasoning"] = raw(caps)
	}
	// A wire-format declaration is meaningful only on this live endpoint.
	// Public model catalogs describe the model, not an intermediary's API.
	if value, present := fields["reasoning_format"]; present {
		if isNull(value) {
			r["reasoning_format"] = raw(nil)
		} else {
			var format models.ReasoningFormat
			if json.Unmarshal(value, &format) != nil || format == "" {
				return nil, invalid("invalid live reasoning_format")
			}
			config := models.Config{ReasoningFormat: format}
			if mapping, present := fields["reasoning_mapping"]; present && !isNull(mapping) {
				config.ReasoningMapping = &models.ReasoningMapping{}
				if json.Unmarshal(mapping, config.ReasoningMapping) != nil {
					return nil, invalid("invalid live reasoning_mapping")
				}
				r["reasoning_mapping"] = mapping
			}
			if _, err := models.ResolveReasoningControls(provider.Protocol, config); err != nil {
				return nil, err
			}
			r["reasoning_format"] = value
		}
	}
	if provider.PresetID == "litellm" {
		if value, present := fields["supports_reasoning"]; present {
			if _, valid := explicitBool(value); isNull(value) || valid {
				if caps == nil {
					caps = map[string]json.RawMessage{}
				}
				caps["supported"] = value
				r["reasoning"] = raw(caps)
			}
		}
		for from, to := range map[string]string{"input_cost_per_token": "input_price", "output_cost_per_token": "output_price"} {
			if value, present := fields[from]; present {
				if price, valid := perMillionTokenPrice(value); valid {
					r[to] = price
				}
			}
		}
	}
	// A null capability container clears the whole recommendation. Reducing it
	// to supported:null would allow lower-priority effort and budget facts to
	// reappear from the public catalog. A null leaf still clears only that leaf.
	reasoningContainers := [][]string{{"reasoning"}, {"reasoning_capabilities"}, {"capabilities", "reasoning"}}
	if provider.PresetID == "floway" {
		reasoningContainers = append(reasoningContainers, []string{"chat", "reasoning"})
	}
	for _, path := range reasoningContainers {
		if value, present := firstJSONField(fields, path); present && isNull(value) {
			r["reasoning"] = raw(nil)
			break
		}
	}
	return r, nil
}

func perMillionTokenPrice(value json.RawMessage) (json.RawMessage, bool) {
	if isNull(value) {
		return value, true
	}
	var number float64
	var text string
	if valueFromJSON(value, &text) {
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, false
		}
		number = parsed
	} else if !valueFromJSON(value, &number) {
		return nil, false
	}
	number *= 1_000_000
	if number < 0 || math.IsInf(number, 0) || math.IsNaN(number) {
		return nil, false
	}
	return raw(number), true
}

// Astral's scalar prices describe the unique base rate. A service-tier or
// long-context rate must not silently become the model's general token price.
func flowayBaseRates(fields map[string]json.RawMessage) map[string]json.RawMessage {
	value, present := firstJSONField(fields, []string{"pricing", "entries"})
	if !present {
		return nil
	}
	var entries []struct {
		Selector map[string]json.RawMessage `json:"selector"`
		Rates    map[string]json.RawMessage `json:"rates"`
	}
	if json.Unmarshal(value, &entries) != nil {
		return nil
	}
	var base map[string]json.RawMessage
	found := false
	for _, entry := range entries {
		if len(entry.Selector) == 0 {
			if found {
				return nil
			}
			found, base = true, entry.Rates
		}
	}
	return base
}

func firstJSONField(fields map[string]json.RawMessage, alternatives ...[]string) (json.RawMessage, bool) {
	for _, path := range alternatives {
		object := fields
		for i, part := range path {
			value, present := object[part]
			if !present {
				break
			}
			if i == len(path)-1 {
				return value, true
			}
			var child map[string]json.RawMessage
			if json.Unmarshal(value, &child) != nil || child == nil {
				break
			}
			object = child
		}
	}
	return nil, false
}
