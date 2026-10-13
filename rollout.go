package astral

import (
	"context"
	"time"
)

// Store appends an entire nonempty batch atomically with compare-and-append.
// Version starts at zero for an absent session and increases once per batch.
// Read must return an isolated copy. ErrNotFound denotes an absent session.
// Acknowledged writes must survive reopening for a durable implementation.
// Core reconciles an ambiguous Append error by reading back the batch's IDs.
// Implementations must never acknowledge partial batches or overwrite entries.
type Store interface {
	Read(context.Context, string) (Rollout, error)
	Append(context.Context, string, uint64, []Entry) error
}

type Rollout struct {
	Version uint64  `json:"version"`
	Entries []Entry `json:"entries"`
}

// Entry is a versioned, typed envelope. Exactly one payload matches Kind.
// History entries form the parent tree. Operational facts reference their
// invocation or queue item instead of changing that tree's parent relation.
type Entry struct {
	ID            string               `json:"id"`
	ParentID      string               `json:"parent_id,omitempty"`
	Kind          EntryKind            `json:"kind"`
	Time          time.Time            `json:"time"`
	RunID         string               `json:"run_id,omitempty"`
	TurnID        string               `json:"turn_id,omitempty"`
	Created       *CreationRecord      `json:"created,omitempty"`
	Configuration *Configuration       `json:"configuration,omitempty"`
	Environment   *EnvironmentSnapshot `json:"environment,omitempty"`
	Message       *Message             `json:"message,omitempty"`
	Tool          *ToolRecord          `json:"tool,omitempty"`
	Approval      *ApprovalRecord      `json:"approval,omitempty"`
	Input         *InputRecord         `json:"input,omitempty"`
	Head          *HeadRecord          `json:"head,omitempty"`
	Compaction    *CompactionRecord    `json:"compaction,omitempty"`
	Attempt       *AttemptRecord       `json:"attempt,omitempty"`
	Run           *RunRecord           `json:"run,omitempty"`
}

type EntryKind string

const (
	EntryCreated           EntryKind = "session_created"
	EntryConfiguration     EntryKind = "configuration"
	EntryEnvironment       EntryKind = "environment"
	EntryMessage           EntryKind = "message"
	EntryToolPrepared      EntryKind = "tool_prepared"
	EntryToolAuthorized    EntryKind = "tool_authorized"
	EntryToolStarted       EntryKind = "tool_started"
	EntryToolCompleted     EntryKind = "tool_completed"
	EntryToolResult        EntryKind = "tool_result"
	EntryToolResolution    EntryKind = "tool_resolution"
	EntryApprovalRequested EntryKind = "approval_requested"
	EntryApprovalDecided   EntryKind = "approval_decided"
	EntryInputQueued       EntryKind = "input_queued"
	EntryInputConsumed     EntryKind = "input_consumed"
	EntryHeadSelected      EntryKind = "head_selected"
	EntryCompaction        EntryKind = "compaction"
	EntryAttempt           EntryKind = "model_attempt"
	EntryRun               EntryKind = "run"
)

func (e Entry) IsHistory() bool {
	switch e.Kind {
	case EntryConfiguration, EntryEnvironment, EntryMessage, EntryToolResult, EntryToolResolution, EntryCompaction:
		return true
	default:
		return false
	}
}

type CreationRecord struct {
	FormatVersion int    `json:"format_version"`
	SessionID     string `json:"session_id"`
	ForkSessionID string `json:"fork_session_id,omitempty"`
	ForkEntryID   string `json:"fork_entry_id,omitempty"`
}

type ToolRecord struct {
	Invocation Invocation    `json:"invocation"`
	Decision   *ToolDecision `json:"decision,omitempty"`
	Result     *ToolResult   `json:"result,omitempty"`
	// An automatically recovered unknown result stops execution until the host
	// explicitly resolves it. A resolution may acknowledge remaining uncertainty.
	NeedsResolution bool `json:"needs_resolution,omitempty"`
}

type ApprovalRecord struct {
	ID          string            `json:"id"`
	ExecutionID string            `json:"execution_id"`
	Request     ApprovalRequest   `json:"request"`
	Decision    *ApprovalDecision `json:"decision,omitempty"`
}

type InputRecord struct {
	ID      string    `json:"id"`
	Kind    InputKind `json:"kind"`
	Message *Message  `json:"message,omitempty"`
}

type HeadRecord struct {
	EntryID string `json:"entry_id"`
}

type CompactionRecord struct {
	ThroughID    string `json:"through_id"`
	Summary      string `json:"summary"`
	ModelID      string `json:"model_id"`
	InputTokens  int    `json:"input_tokens"`
	Usage        Usage  `json:"usage,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

type AttemptRecord struct {
	ID              string `json:"id"`
	ModelID         string `json:"model_id"`
	ConfigurationID string `json:"configuration_id"`
	HeadID          string `json:"head_id"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
	Usage           Usage  `json:"usage,omitempty"`
	Purpose         string `json:"purpose,omitempty"`
}

type RunRecord struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}
