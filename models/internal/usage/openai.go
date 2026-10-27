// Package usage normalizes provider-reported token counts without inferring
// missing counts from totals or treating an omitted field as a reported zero.
package usage

import core "github.com/oines/astral-core"

// OpenAI accepts the two documented OpenAI usage shapes. Compatible services
// sometimes return Chat names on Responses or Responses names on Chat.
type OpenAI struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	InputTokens      *int `json:"input_tokens"`
	OutputTokens     *int `json:"output_tokens"`

	PromptDetails     *inputDetails  `json:"prompt_tokens_details"`
	CompletionDetails *outputDetails `json:"completion_tokens_details"`
	InputDetails      *inputDetails  `json:"input_tokens_details"`
	OutputDetails     *outputDetails `json:"output_tokens_details"`
	PromptCacheHit    *int           `json:"prompt_cache_hit_tokens"`
}

type inputDetails struct {
	CachedTokens     *int `json:"cached_tokens"`
	CacheWriteTokens *int `json:"cache_write_tokens"`
}

type outputDetails struct {
	ReasoningTokens *int `json:"reasoning_tokens"`
}

func first(values ...*int) *int {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func (u OpenAI) Chat() core.Usage {
	return normalized(first(u.PromptTokens, u.InputTokens), first(u.CompletionTokens, u.OutputTokens),
		u.PromptDetails, u.InputDetails, u.CompletionDetails, u.OutputDetails, u.PromptCacheHit)
}

func (u OpenAI) Responses() core.Usage {
	return normalized(first(u.InputTokens, u.PromptTokens), first(u.OutputTokens, u.CompletionTokens),
		u.InputDetails, u.PromptDetails, u.OutputDetails, u.CompletionDetails, u.PromptCacheHit)
}

func normalized(input, output *int, inputPrimary, inputAlias *inputDetails, outputPrimary, outputAlias *outputDetails, cachedAlias *int) core.Usage {
	result := core.Usage{Available: true, InputMissing: input == nil, OutputMissing: output == nil}
	if input != nil {
		result.InputTokens = *input
	}
	if output != nil {
		result.OutputTokens = *output
	}
	for _, details := range []*inputDetails{inputPrimary, inputAlias} {
		if details != nil {
			result.CachedInputTokens = first(result.CachedInputTokens, details.CachedTokens)
			result.CacheWriteTokens = first(result.CacheWriteTokens, details.CacheWriteTokens)
		}
	}
	result.CachedInputTokens = first(result.CachedInputTokens, cachedAlias)
	for _, details := range []*outputDetails{outputPrimary, outputAlias} {
		if details != nil {
			result.ReasoningTokens = first(result.ReasoningTokens, details.ReasoningTokens)
		}
	}
	return result
}
