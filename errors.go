package astral

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound                = errors.New("astral: session or entry not found")
	ErrConflict                = errors.New("astral: store version conflict")
	ErrBusy                    = errors.New("astral: session has an active operation")
	ErrClosed                  = errors.New("astral: agent is closed")
	ErrInvalid                 = errors.New("astral: invalid input")
	ErrCorrupt                 = errors.New("astral: invalid rollout")
	ErrIncompleteBatch         = errors.New("astral: incomplete tool call/result boundary")
	ErrOutcomeUnknown          = errors.New("astral: tool outcome requires host resolution")
	ErrEnvironmentRefresh      = errors.New("astral: refresh the environment before continuing")
	ErrBudget                  = errors.New("astral: request exceeds model context budget")
	ErrCompactionInputTooLarge = errors.New("astral: compaction input cannot fit")
	ErrCompactionNoProgress    = errors.New("astral: compaction cannot make progress")
	ErrIncompleteResponse      = errors.New("astral: model response did not finish successfully")
	ErrSlowSubscriber          = errors.New("astral: subscriber overflow; reload durable history")
	ErrTurnLimit               = errors.New("astral: run turn limit reached")
	ErrAlreadyResolved         = errors.New("astral: decision or tool already resolved")
)

// ModelError lets adapters classify failures without parsing provider messages.
// Authentication, invalid requests and cancellation should not be transient.
type ModelError struct {
	Code       string
	Message    string
	Transient  bool
	RetryAfter time.Duration
	Cause      error
}

func (e *ModelError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "model request failed: " + e.Code
}
func (e *ModelError) Unwrap() error { return e.Cause }

// ToolError lets a host classify effects when a backend can return only an
// error. For example, a lost remote acknowledgement may be OutcomeUnknown even
// though the transport error itself is retryable. Core never retries the tool.
type ToolError struct {
	Outcome Outcome
	Code    string
	Message string
	Cause   error
}

func (e *ToolError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "tool execution failed: " + e.Code
}
func (e *ToolError) Unwrap() error { return e.Cause }

// BudgetError reports the estimate for the exact request that was rejected.
type BudgetError struct{ Input, Output, Margin, Window int }

func (e *BudgetError) Error() string {
	return fmt.Sprintf("%v: %d input + %d output + %d margin > %d", ErrBudget, e.Input, e.Output, e.Margin, e.Window)
}
func (e *BudgetError) Unwrap() error { return ErrBudget }

// UnknownOutcomeError identifies a tool whose effects cannot be inferred safely.
type UnknownOutcomeError struct{ ExecutionID string }

func (e *UnknownOutcomeError) Error() string {
	return fmt.Sprintf("%v: %s", ErrOutcomeUnknown, e.ExecutionID)
}
func (e *UnknownOutcomeError) Unwrap() error { return ErrOutcomeUnknown }
