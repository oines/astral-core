package session

import (
	"time"

	protocol "github.com/oines/astral-core"
)

// Entry is a versioned, typed envelope. Exactly one payload matches Kind.
// History entries form the parent tree. Operational facts reference their
// invocation or queue item instead of changing that tree's parent relation.
type Entry struct {
	SessionID     string              `json:"session_id"`
	Sequence      uint64              `json:"sequence"`
	Version       uint64              `json:"version"`
	Usage         protocol.Usage      `json:"usage,omitempty"`
	ID            string              `json:"id"`
	ParentID      string              `json:"parent_id,omitempty"`
	Kind          EntryKind           `json:"kind"`
	Time          time.Time           `json:"time"`
	InputID       string              `json:"input_id,omitempty"`
	RunID         string              `json:"run_id,omitempty"`
	TurnID        string              `json:"turn_id,omitempty"`
	Created       *CreationRecord     `json:"created,omitempty"`
	Configuration *Configuration      `json:"configuration,omitempty"`
	Message       *protocol.Message   `json:"message,omitempty"`
	Tool          *ToolRecord         `json:"tool,omitempty"`
	Approval      *ApprovalRecord     `json:"approval,omitempty"`
	Input         *InputRecord        `json:"input,omitempty"`
	Compaction    *CompactionRecord   `json:"compaction,omitempty"`
	Clear         *InputClear         `json:"clear,omitempty"`
	Turn          *protocol.TurnState `json:"turn,omitempty"`
	Run           *RunRecord          `json:"run,omitempty"`
}

type EntryKind string

const (
	EntryCreated           EntryKind = "session_created"
	EntryConfiguration     EntryKind = "configuration"
	EntryContext           EntryKind = "context"
	EntryMessage           EntryKind = "message"
	EntryToolPrepared      EntryKind = "tool_prepared"
	EntryToolAuthorized    EntryKind = "tool_authorized"
	EntryToolStarted       EntryKind = "tool_started"
	EntryToolCompleted     EntryKind = "tool_completed"
	EntryToolResult        EntryKind = "tool_result"
	EntryToolResolution    EntryKind = "tool_resolution"
	EntryToolControl       EntryKind = "tool_control"
	EntryToolUnresolved    EntryKind = "tool_unresolved"
	EntryApprovalRequested EntryKind = "approval_requested"
	EntryApprovalDecided   EntryKind = "approval_decided"
	EntryInputQueued       EntryKind = "input_queued"
	EntryInputConsumed     EntryKind = "input_consumed"
	EntryInputCleared      EntryKind = "input_cleared"
	EntryTurn              EntryKind = "turn"
	EntryCompaction        EntryKind = "compaction"
	EntryRun               EntryKind = "run"
)

func (e Entry) IsHistory() bool {
	switch e.Kind {
	case EntryConfiguration, EntryContext, EntryMessage, EntryToolResult, EntryToolResolution, EntryToolControl, EntryToolUnresolved, EntryCompaction:
		return true
	default:
		return false
	}
}

type CreationRecord struct {
	ForkDigest    string      `json:"fork_digest,omitempty"`
	RolloutID     string      `json:"rollout_id"`
	ForkRolloutID string      `json:"fork_rollout_id,omitempty"`
	ForkMode      ForkMode    `json:"fork_mode,omitempty"`
	Projection    *Projection `json:"projection,omitempty"`
	FormatVersion int         `json:"format_version"`
	SessionID     string      `json:"session_id"`
	ForkSessionID string      `json:"fork_session_id,omitempty"`
	ForkEntryID   string      `json:"fork_entry_id,omitempty"`
}

type ToolRecord struct {
	Invocation protocol.Invocation    `json:"invocation"`
	Decision   *protocol.ToolDecision `json:"decision,omitempty"`
	Result     *protocol.ToolResult   `json:"result,omitempty"`
	Control    *protocol.RunEnd       `json:"control,omitempty"`
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

type InputRecord = protocol.QueuedInput
type InputClear struct {
	Kinds []InputKind `json:"kinds"`
}

type CompactionRecord struct {
	ThroughID    string         `json:"through_id"`
	Summary      string         `json:"summary"`
	ModelID      string         `json:"model_id"`
	InputTokens  int            `json:"input_tokens"`
	Usage        protocol.Usage `json:"usage,omitempty"`
	Instructions string         `json:"instructions,omitempty"`
}

type RunRecord struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	EndReason string `json:"end_reason,omitempty"`
	QueueOnly bool   `json:"queue_only,omitempty"`
}
