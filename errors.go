package astral

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrBusy               = errors.New("astral: session has an active operation")
	ErrClosed             = errors.New("astral: agent is closed")
	ErrInvalid            = errors.New("astral: invalid input")
	ErrIncompleteBatch    = errors.New("astral: incomplete tool call/result boundary")
	ErrOutcomeUnknown     = errors.New("astral: tool outcome requires host resolution")
	ErrIncompleteResponse = errors.New("astral: model response did not finish successfully")
	ErrSlowSubscriber     = errors.New("astral: subscriber overflow; reload durable history")
	ErrTurnLimit          = errors.New("astral: run turn limit reached")
	ErrNotContinuable     = errors.New("astral: no unfinished work or eligible queued input to continue")
)

// ModelError lets adapters classify failures without parsing provider messages.
// Authentication, invalid requests and cancellation should not be transient.
type ModelError struct {
	Code    string
	Message string
	// ContextOverflow permits one compaction recovery at a complete boundary.
	ContextOverflow bool
	Transient       bool
	RetryAfter      time.Duration
	Cause           error
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

// UnknownOutcomeError identifies a tool whose effects cannot be inferred safely.
type UnknownOutcomeError struct{ ExecutionID string }

func (e *UnknownOutcomeError) Error() string {
	return fmt.Sprintf("%v: %s", ErrOutcomeUnknown, e.ExecutionID)
}
func (e *UnknownOutcomeError) Unwrap() error { return ErrOutcomeUnknown }
