package completions

import (
	"encoding/json"

	"github.com/oines/astral-core/models/internal/usage"
)

type chatReasoningDetail map[string]any

type chatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// --- Response types ---

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

type chatChoice struct {
	Index        int             `json:"index"`
	Message      chatRespMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type chatRespMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// ReasoningContent is a pointer because the key's presence is itself a
	// signal: DeepSeek returns "" for a thinking-mode step that produced no
	// reasoning and omits the key when thinking is disabled.
	ReasoningContent *string               `json:"reasoning_content,omitempty"`
	ReasoningDetails []chatReasoningDetail `json:"reasoning_details,omitempty"`
	Reasoning        string                `json:"reasoning,omitempty"`
	Refusal          string                `json:"refusal,omitempty"`
	ToolCalls        []chatToolCall        `json:"tool_calls,omitempty"`
	Images           []chatImagePart       `json:"images,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatUsage = usage.OpenAI

// --- Streaming chunk types (chat.completion.chunk) ---

type chatChunkResponse struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   *chatUsage        `json:"usage,omitempty"`
}

type chatChunkChoice struct {
	Index        int            `json:"index"`
	Delta        chatChunkDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type chatChunkDelta struct {
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// ReasoningContent is a pointer for the same reason as in chatRespMessage:
	// DeepSeek's first thinking-mode delta carries "" and later deltas null.
	ReasoningContent *string               `json:"reasoning_content,omitempty"`
	ReasoningDetails []chatReasoningDetail `json:"reasoning_details,omitempty"`
	Reasoning        string                `json:"reasoning,omitempty"`
	Refusal          string                `json:"refusal,omitempty"`
	ToolCalls        []chatToolCallChunk   `json:"tool_calls,omitempty"`
	Images           []chatImagePart       `json:"images,omitempty"`
}

type chatImagePart struct {
	Type     string       `json:"type"`
	ImageURL chatImageURL `json:"image_url"`
}

type chatToolCallChunk struct {
	Index    int               `json:"index"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function chatFunctionDelta `json:"function,omitempty"`
}

type chatFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
