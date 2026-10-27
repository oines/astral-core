// Package models supplies provider configuration and diagnostics for Astral.
// Providers implement astral.Model and use Core's messages directly.
package models

import (
	"context"
	"net/http"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/models/internal/reqheaders"
)

type Dialect string

const (
	OpenAI     Dialect = "openai"
	DeepSeek   Dialect = "deepseek"
	Qwen       Dialect = "qwen"
	OpenRouter Dialect = "openrouter"
	Compatible Dialect = "compatible"
	Anthropic  Dialect = "anthropic"
	Google     Dialect = "google"
	MiMo       Dialect = "mimo"
)

// Config binds one immutable model profile to a provider connection.
// Spec.ID is an optional caller identity, defaulting to the remote Model ID.
// Zero token limits mean unknown for direct calls; Core sessions require known
// positive context/output limits. Model is the remote API's model identifier.
// Credentials and HTTP transport are never copied into Core history.
type Config struct {
	Spec        astral.ModelSpec
	Model       string
	BaseURL     string
	APIKey      string            `json:"-"`
	Headers     map[string]string `json:"-"`
	HTTPClient  *http.Client      `json:"-"`
	Dialect     Dialect
	ReplayScope string
	Streaming   *bool
	Reasoning   *ReasoningCapabilities
	// ReasoningFormat controls only reasoning parameters, independently of the
	// connection's other dialect behavior. Empty selects the protocol default.
	ReasoningFormat  ReasoningFormat
	ReasoningMapping *ReasoningMapping
	// RequireAdaptiveEffort declares an API constraint: adaptive thinking
	// requires an explicit output effort. A generic "on" needs a declared
	// OnEffort binding; it must not silently select an arbitrary effort.
	RequireAdaptiveEffort bool
	// ResponsesCompatibility declares verified differences from the standard
	// Responses request contract. It has no effect on other protocols.
	ResponsesCompatibility *ResponsesCompatibility
	// ReasoningReplay selects the assistant field used by Chat Completions:
	// reasoning_content, reasoning_details, or reasoning. Empty uses the dialect.
	ReasoningReplay        string
	RequireReasoningReplay bool
	// ImageTokenBudget overrides the content estimator's 1200-token image
	// approximation. Encoded image bytes are never counted as text tokens.
	// Images and ToolCalling are sparse capability facts. Empty/unknown do not
	// forbid explicit input; unsupported rejects new image/tool declarations.
	Images           string
	ToolCalling      string
	ImageTokenBudget int
	// Codex selects the subscription Responses endpoint, which requires SSE
	// and does not expose the public API's max_output_tokens parameter.
	Codex bool
}

// ResponsesCompatibility controls optional fields automatically added by the
// Responses encoder. Nil retains standard behavior. A false capability omits
// that default and rejects the unsupported field or value in ProviderOptions.
// This never removes native response items or their encrypted replay data.
type ResponsesCompatibility struct {
	SupportsInclude *bool `json:"supports_include,omitempty"`
	SupportsStore   *bool `json:"supports_store,omitempty"`
	// SupportsEncryptedReasoningInclude refines SupportsInclude when the
	// route accepts include but not its reasoning.encrypted_content value.
	SupportsEncryptedReasoningInclude *bool `json:"supports_encrypted_reasoning_include,omitempty"`
}

// ReasoningCapabilities describes a specific connection/protocol/model route.
// Nil means unknown. An empty non-nil Efforts list means no effort control.
// No implicit low/medium/high list or effort-to-budget mapping is introduced.
type ReasoningCapabilities struct {
	Supported     *bool    `json:"supported,omitempty"`
	Toggle        *bool    `json:"toggle,omitempty"`
	Disable       *bool    `json:"disable,omitempty"`
	DefaultOn     *bool    `json:"default_on,omitempty"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	Efforts       []string `json:"efforts"`
	Budget        *bool    `json:"budget,omitempty"`
	DynamicBudget *bool    `json:"dynamic_budget,omitempty"`
	MinBudget     *int     `json:"min_budget,omitempty"`
	MaxBudget     *int     `json:"max_budget,omitempty"`
	OffEffort     string   `json:"off_effort,omitempty"`
	OnEffort      string   `json:"on_effort,omitempty"`
}

// ModelInfo is discovery data, not a claim that a model passed tool/replay tests.
type ModelInfo struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

func WithClientRequestID(ctx context.Context, id string) context.Context {
	return reqheaders.WithClientRequestID(ctx, id)
}

// WithRequestHeaders adds host-owned request metadata, such as a stable coding
// session ID. Headers are copied and never become part of model history.
func WithRequestHeaders(ctx context.Context, headers map[string]string) context.Context {
	return reqheaders.WithContext(ctx, headers)
}
