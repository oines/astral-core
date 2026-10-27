package models

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ParseReasoningMetadata reads one model-list record into sparse capability
// fields. Missing values stay absent; explicit null, false and [] survive
// recommendation merging. It never selects a request format or a user default.
func ParseReasoningMetadata(source string, raw json.RawMessage) (map[string]json.RawMessage, error) {
	out, err := parseReasoningMetadata(source, raw)
	if err != nil {
		return nil, err
	}
	var record map[string]json.RawMessage
	_ = json.Unmarshal(raw, &record)
	containers := []json.RawMessage{record["reasoning"]}
	if source == "floway" {
		var chat map[string]json.RawMessage
		_ = json.Unmarshal(record["chat"], &chat)
		containers = append(containers, chat["reasoning"])
	}
	var capabilities map[string]json.RawMessage
	_ = json.Unmarshal(record["capabilities"], &capabilities)
	containers = append(containers, capabilities["reasoning"], record["reasoning_capabilities"])
	for _, data := range containers {
		if len(data) == 0 {
			continue
		}
		if string(data) == "null" {
			out["supported"] = json.RawMessage("null")
			continue
		}
		var supported bool
		if json.Unmarshal(data, &supported) == nil {
			out["supported"] = append(json.RawMessage(nil), data...)
			continue
		}
		var info map[string]json.RawMessage
		if json.Unmarshal(data, &info) != nil || info == nil {
			return nil, reasoningError("metadata.reasoning", "expected a boolean, object or null")
		}
		for _, key := range []string{"supported", "toggle", "disable", "default_on", "default_effort", "efforts", "budget", "dynamic_budget", "min_budget", "max_budget", "on_effort", "off_effort"} {
			value, present := info[key]
			if !present {
				continue
			}
			// Decode each explicit field through the public capability type.
			// This preserves null/false/[] without accepting an invalid type.
			check, _ := json.Marshal(map[string]json.RawMessage{key: value})
			var caps ReasoningCapabilities
			if err := json.Unmarshal(check, &caps); err != nil {
				return nil, reasoningError("metadata."+key, "invalid capability value")
			}
			for _, effort := range caps.Efforts {
				if effort == "" {
					return nil, reasoningError("metadata."+key, "effort values must be nonempty")
				}
			}
			for _, effort := range append(append([]string{}, caps.Efforts...), caps.DefaultEffort, caps.OnEffort, caps.OffEffort) {
				if strings.TrimSpace(effort) != effort {
					return nil, reasoningError("metadata."+key, "effort values must have no surrounding whitespace")
				}
			}
			out[key] = append(json.RawMessage(nil), value...)
		}
		var mandatory bool
		if value, present := info["mandatory"]; present && string(value) != "null" {
			if json.Unmarshal(value, &mandatory) != nil {
				return nil, reasoningError("metadata.mandatory", "expected a boolean")
			}
			if mandatory {
				for key, value := range map[string]bool{"toggle": false, "disable": false, "default_on": true} {
					if _, explicit := info[key]; !explicit {
						out[key], _ = json.Marshal(value)
					}
				}
			}
		}
	}
	return out, nil
}

func parseReasoningMetadata(source string, raw json.RawMessage) (map[string]json.RawMessage, error) {
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil || record == nil {
		return nil, reasoningError("metadata", "expected a model object")
	}
	out := map[string]json.RawMessage{}
	put := func(key string, value any) { out[key], _ = json.Marshal(value) }
	read := func(fields map[string]json.RawMessage, key string, target any) (bool, error) {
		value, found := fields[key]
		if !found || string(value) == "null" {
			return false, nil
		}
		if err := json.Unmarshal(value, target); err != nil {
			return false, reasoningError("metadata."+key, fmt.Sprintf("invalid value: %v", err))
		}
		return true, nil
	}
	readEfforts := func(fields map[string]json.RawMessage, key string) error {
		if string(fields[key]) == "null" {
			out["efforts"] = json.RawMessage("null")
			return nil
		}
		var values []string
		if found, err := read(fields, key, &values); err != nil {
			return err
		} else if found {
			for _, value := range values {
				if value == "" || strings.TrimSpace(value) != value {
					return reasoningError("metadata."+key, "effort values must be nonempty without surrounding whitespace")
				}
			}
			put("efforts", values)
		}
		return nil
	}
	readDefault := func(fields map[string]json.RawMessage, key string) error {
		if string(fields[key]) == "null" {
			out["default_effort"] = json.RawMessage("null")
			return nil
		}
		var value string
		if found, err := read(fields, key, &value); err != nil {
			return err
		} else if found && value != "" {
			put("default_effort", value)
		}
		return nil
	}
	readBool := func(fields map[string]json.RawMessage, key, target string) error {
		if string(fields[key]) == "null" {
			put(target, nil)
			return nil
		}
		var value bool
		if found, err := read(fields, key, &value); err != nil {
			return err
		} else if found {
			put(target, value)
		}
		return nil
	}
	if source == "magpie" {
		if err := readBool(record, "reasoning", "supported"); err != nil {
			return nil, err
		}
		// The string list and object list name the same native
		// vocabulary. An explicitly present object list takes precedence.
		if _, present := record["supported_reasoning_levels"]; !present {
			if err := readEfforts(record, "reasoning_levels"); err != nil {
				return nil, err
			}
		}
	}

	switch source {
	case "floway":
		var chat, info map[string]json.RawMessage
		if _, err := read(record, "chat", &chat); err != nil {
			return nil, err
		}
		if string(chat["reasoning"]) == "null" {
			put("supported", nil)
			return out, nil
		}
		if present, err := read(chat, "reasoning", &info); err != nil {
			return nil, err
		} else if !present {
			return out, nil
		}
		put("supported", true)
		var effort, budget map[string]json.RawMessage
		if _, err := read(info, "effort", &effort); err != nil {
			return nil, err
		}
		if err := readEfforts(effort, "supported"); err != nil {
			return nil, err
		}
		if err := readDefault(effort, "default"); err != nil {
			return nil, err
		}
		if string(info["budget_tokens"]) == "null" {
			put("budget", nil)
		} else if present, err := read(info, "budget_tokens", &budget); err != nil {
			return nil, err
		} else if present {
			put("budget", true)
			for _, bound := range []string{"min", "max"} {
				var value int
				if string(budget[bound]) == "null" {
					put(bound+"_budget", nil)
				} else if found, err := read(budget, bound, &value); err != nil {
					return nil, err
				} else if found {
					put(bound+"_budget", value)
				}
			}
		}
		var mandatory bool
		if _, err := read(info, "mandatory", &mandatory); err != nil {
			return nil, err
		} else if mandatory {
			put("toggle", false)
			put("disable", false)
			put("default_on", true)
		}
		// adaptive describes model-controlled depth; it is not Gemini's
		// negative budget sentinel and does not prove a user-facing toggle.
	case "openrouter":
		var info map[string]json.RawMessage
		present, err := read(record, "reasoning", &info)
		if err != nil {
			return nil, err
		}
		if present && info != nil {
			put("supported", true)
			var mandatory bool
			if found, err := read(info, "mandatory", &mandatory); err != nil {
				return nil, err
			} else if found {
				put("toggle", !mandatory)
				put("disable", !mandatory)
				if mandatory {
					put("default_on", true)
				}
			}
			var enabled bool
			if string(info["default_enabled"]) == "null" {
				put("default_on", nil)
			} else if found, err := read(info, "default_enabled", &enabled); err != nil {
				return nil, err
			} else if found {
				put("default_on", enabled)
			}
			// This source explicitly defines null as the gateway's complete
			// effort vocabulary, and omission as no effort control.
			if value, found := info["supported_efforts"]; found && string(value) == "null" {
				put("efforts", []string{"max", "xhigh", "high", "medium", "low", "minimal", "none"})
			} else if found {
				if err := readEfforts(info, "supported_efforts"); err != nil {
					return nil, err
				}
			} else {
				put("efforts", []string{})
			}
			var efforts []string
			_ = json.Unmarshal(out["efforts"], &efforts)
			if mandatory {
				efforts = slices.DeleteFunc(efforts, func(value string) bool { return value == "none" })
				put("efforts", efforts)
			} else if slices.Contains(efforts, "none") {
				put("off_effort", "none")
				put("disable", true)
			}
			var budget bool
			if _, err := read(info, "supports_max_tokens", &budget); err != nil {
				return nil, err
			}
			if string(info["supports_max_tokens"]) == "null" {
				put("budget", nil)
			} else {
				put("budget", budget)
			}
			if err := readDefault(info, "default_effort"); err != nil {
				return nil, err
			}
			if string(out["default_effort"]) == `"none"` && out["default_on"] == nil {
				put("default_on", false)
			}
			return out, nil
		}
		// Older gateways expose parameter names, but these do not establish
		// an effort enumeration or whether an independent toggle exists.
		var parameters []string
		if _, err := read(record, "supported_parameters", &parameters); err != nil {
			return nil, err
		}
		if slices.Contains(parameters, "reasoning") || slices.Contains(parameters, "reasoning_effort") {
			put("supported", true)
		}
		var defaults map[string]json.RawMessage
		if _, err := read(record, "default_parameters", &defaults); err != nil {
			return nil, err
		}
		if err := readDefault(defaults, "reasoning_effort"); err != nil {
			return nil, err
		}
	case "github-copilot", "copilot":
		if err := readEfforts(record, "supported_reasoning_effort"); err != nil {
			return nil, err
		}
		var capabilities struct {
			Supports map[string]json.RawMessage `json:"supports"`
		}
		if _, err := read(record, "capabilities", &capabilities); err != nil {
			return nil, err
		}
		var supported bool
		if string(capabilities.Supports["reasoning"]) == "null" {
			put("supported", nil)
		} else if found, err := read(capabilities.Supports, "reasoning", &supported); err != nil {
			return nil, err
		} else if found {
			put("supported", supported)
		}
	default:
		// DeepSeek's effort object and Codex's reasoning-level records also
		// occur on compatible model catalogs. Their exact fields are safe
		// to read without guessing a provider from a model's name.
		var effort map[string]json.RawMessage
		if _, err := read(record, "effort", &effort); err != nil {
			return nil, err
		}
		if err := readEfforts(effort, "supported_levels"); err != nil {
			return nil, err
		}
		if err := readDefault(effort, "default_level"); err != nil {
			return nil, err
		}
		var levels []struct {
			Effort string `json:"effort"`
		}
		if string(record["supported_reasoning_levels"]) == "null" {
			put("efforts", nil)
		} else if found, err := read(record, "supported_reasoning_levels", &levels); err != nil {
			return nil, err
		} else if found {
			values := make([]string, 0, len(levels))
			for _, level := range levels {
				if level.Effort == "" || strings.TrimSpace(level.Effort) != level.Effort {
					return nil, reasoningError("metadata.supported_reasoning_levels", "invalid effort")
				}
				values = append(values, level.Effort)
			}
			put("efforts", values)
		}
		if err := readDefault(record, "default_reasoning_level"); err != nil {
			return nil, err
		}
	}
	return out, nil
}
