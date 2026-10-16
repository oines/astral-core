package astral

import (
	"context"
	"time"
)

// Config contains host-owned capabilities. Driver is optional; by default the
// Agent keeps process-local conversation state, without persistence guarantees.
type Config struct {
	ID               string
	Model            Model
	SystemPrompt     string
	Tools            []Tool
	ResolveModel     func(context.Context, string) (Model, error)
	BeforeToolCall   BeforeToolCall
	AfterToolCall    AfterToolCall
	FinishTurn       FinishTurn
	Fallback         func(context.Context, ModelFailure) (Fallback, error)
	Retry            RetryPolicy
	MaxTurns         int
	MaxParallelTools int
	// MaxSteeringPerTurn defaults to 1 and accepts 1–16 queued messages per
	// boundary. Follow-ups are always consumed one at a time.
	MaxSteeringPerTurn int
	Driver             Driver
}

type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	RequestTimeout time.Duration
	MaxFallbacks   int
}
