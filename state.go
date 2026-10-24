package astral

import (
	"fmt"
	"math"
	"time"
)

// Projection contains branch-local references and unfinished protocol work.
// It never embeds completed conversation history or a queue backlog.
type Projection struct {
	HeadID               string                `json:"head_id,omitempty"`
	ConfigurationID      string                `json:"configuration_id,omitempty"`
	CompactionID         string                `json:"compaction_id,omitempty"`
	ThroughID            string                `json:"through_id,omitempty"`
	LastMessageID        string                `json:"last_message_id,omitempty"`
	BatchConfigurationID string                `json:"batch_configuration_id,omitempty"`
	ToolEntries          map[string]string     `json:"tool_entries,omitempty"`
	PendingCalls         map[string]ToolCall   `json:"pending_calls,omitempty"`
	Unresolved           map[string]ToolRecord `json:"unresolved,omitempty"`
}

func (p Projection) Complete() bool { return len(p.PendingCalls) == 0 && len(p.Unresolved) == 0 }

// ToolState retains only the current assistant batch's execution facts.
// Exact runtime checkpoints are distinct from model-generated summaries.
type ToolState struct {
	Prepared   *Entry `json:"prepared,omitempty"`
	Authorized *Entry `json:"authorized,omitempty"`
	Started    *Entry `json:"started,omitempty"`
	Completed  *Entry `json:"completed,omitempty"`
	Result     *Entry `json:"result,omitempty"`
	Control    *Entry `json:"control,omitempty"`
	Unresolved *Entry `json:"unresolved,omitempty"`
}
type RuntimeState struct {
	FormatVersion int                       `json:"format_version"`
	RolloutID     string                    `json:"rollout_id"`
	SessionID     string                    `json:"session_id"`
	Version       uint64                    `json:"version"`
	Creation      CreationRecord            `json:"creation"`
	Branch        Projection                `json:"branch"`
	Tools         map[string]ToolState      `json:"tools,omitempty"`
	Approvals     map[string]ApprovalRecord `json:"approvals,omitempty"`
	PendingInputs map[InputKind]uint64      `json:"pending_inputs,omitempty"`
	LastRun       *RunRecord                `json:"last_run,omitempty"`
	// CurrentAttemptID pins the unfinished generation across process restarts.
	// A committed reply or acknowledged terminal Run clears the pointer.
	CurrentAttemptID string `json:"current_attempt_id,omitempty"`
}

// Reduce is the single state transition used by live commits, backend replay,
// and checkpoint verification. Its cost depends on this batch and unfinished
// work, never on the total number of historical records.
func Reduce(previous RuntimeState, c Commit) (RuntimeState, []Record, error) {
	if c.ID == "" || len(c.Entries)+len(c.Attempts) == 0 || c.ExpectedVersion != previous.Version || previous.Version == 0 && len(c.Entries) == 0 {
		return RuntimeState{}, nil, ErrConflict
	}
	if previous.Version > 0 && !SupportsFormat(previous.FormatVersion) {
		return RuntimeState{}, nil, ErrUnsupportedFormat
	}
	state, err := copyJSON(previous)
	if err != nil {
		return RuntimeState{}, nil, err
	}
	records := make([]Record, 0, len(c.Entries))
	ids := map[string]bool{}
	var lastSequence uint64
	for entryIndex, entry := range c.Entries {
		e, err := copyJSON(entry)
		if err != nil {
			return RuntimeState{}, nil, err
		}
		if e.Kind == EntryCreated && e.Created != nil && !SupportsFormat(e.Created.FormatVersion) {
			return RuntimeState{}, nil, ErrUnsupportedFormat
		}
		if err = validateEnvelope(e); err != nil {
			return RuntimeState{}, nil, err
		}
		if e.ID == "" || ids[e.ID] || e.Sequence == 0 || (lastSequence != 0 && e.Sequence != lastSequence+1) || e.Version != previous.Version+1 {
			return RuntimeState{}, nil, fmt.Errorf("%w: entry identity, sequence or version", ErrCorrupt)
		}
		ids[e.ID] = true
		lastSequence = e.Sequence
		if previous.Version == 0 && len(records) == 0 {
			if e.Kind != EntryCreated || !SupportsFormat(e.Created.FormatVersion) {
				return RuntimeState{}, nil, ErrUnsupportedFormat
			}
			state.FormatVersion = e.Created.FormatVersion
			state.SessionID = e.Created.SessionID
			state.RolloutID = e.Created.RolloutID
			if e.Created.Projection != nil {
				if e.Created.ForkEntryID == "" || e.Created.Projection.HeadID != e.Created.ForkEntryID || !e.Created.Projection.Complete() {
					return RuntimeState{}, nil, ErrCorrupt
				}
				state.Branch = clone(*e.Created.Projection)
			} else if e.Created.ForkEntryID != "" {
				return RuntimeState{}, nil, ErrCorrupt
			}
			state.Creation = *e.Created
		} else if e.Kind == EntryCreated {
			return RuntimeState{}, nil, fmt.Errorf("%w: duplicate creation", ErrCorrupt)
		}
		if e.SessionID != state.SessionID {
			return RuntimeState{}, nil, fmt.Errorf("%w: entry belongs to another session", ErrCorrupt)
		}
		if e.IsHistory() {
			if e.ParentID != state.Branch.HeadID {
				return RuntimeState{}, nil, fmt.Errorf("%w: history parent is not the selected head", ErrCorrupt)
			}
			state.Branch.HeadID = e.ID
		} else if e.ParentID != "" {
			return RuntimeState{}, nil, fmt.Errorf("%w: operational parent", ErrCorrupt)
		}
		switch e.Kind {
		case EntryConfiguration:
			state.Branch.ConfigurationID = e.ID
		case EntryContext:
			if len(state.Branch.PendingCalls) > 0 {
				return RuntimeState{}, nil, ErrIncompleteBatch
			}
		case EntryMessage:
			if len(state.Branch.PendingCalls) > 0 {
				return RuntimeState{}, nil, ErrIncompleteBatch
			}
			if len(state.Branch.Unresolved) > 0 {
				return RuntimeState{}, nil, ErrOutcomeUnknown
			}
			state.Branch.LastMessageID = e.ID
			if e.Message.Role == RoleAssistant {
				state.CurrentAttemptID = ""
			}
			state.Branch.BatchConfigurationID = state.Branch.ConfigurationID
			if e.Generation != nil {
				// Model selection may advance the branch while a request is in
				// flight. Its tool batch belongs to the configuration it actually saw.
				state.Branch.BatchConfigurationID = e.Generation.ConfigurationID
			}
			state.Branch.PendingCalls = nil
			state.Branch.ToolEntries = nil
			state.Tools = nil
			state.Approvals = nil
			if calls := e.Message.ToolCalls(); len(calls) > 0 && !e.Message.Failed() {
				state.Branch.PendingCalls = map[string]ToolCall{}
				for _, call := range calls {
					state.Branch.PendingCalls[call.ID] = call
				}
			}
		case EntryToolPrepared, EntryToolAuthorized, EntryToolStarted, EntryToolCompleted, EntryToolResult, EntryToolResolution, EntryToolControl, EntryToolUnresolved:
			inv := e.Tool.Invocation
			_, resolvingControl := state.Branch.Unresolved[inv.ExecutionID]
			resolvingControl = resolvingControl && e.Kind == EntryToolControl
			if e.Kind == EntryToolResolution || resolvingControl {
				old, ok := state.Branch.Unresolved[inv.ExecutionID]
				if !ok || !sameJSON(old.Invocation, inv) {
					return RuntimeState{}, nil, fmt.Errorf("%w: unresolved execution missing", ErrCorrupt)
				}
				delete(state.Branch.Unresolved, inv.ExecutionID)
				if len(state.Branch.Unresolved) == 0 {
					state.Branch.Unresolved = nil
				}
			} else {
				call, ok := state.Branch.PendingCalls[inv.Call.ID]
				if !ok || inv.AssistantID != state.Branch.LastMessageID || !sameJSON(call, inv.Call) {
					return RuntimeState{}, nil, fmt.Errorf("%w: tool fact outside current batch", ErrCorrupt)
				}
			}
			if state.Tools == nil {
				state.Tools = map[string]ToolState{}
			}
			work := state.Tools[inv.Call.ID]
			if work.Prepared != nil && !sameJSON(work.Prepared.Tool.Invocation, inv) {
				return RuntimeState{}, nil, fmt.Errorf("%w: pinned invocation changed", ErrCorrupt)
			}
			switch e.Kind {
			case EntryToolPrepared:
				if work.Prepared != nil {
					return RuntimeState{}, nil, ErrCorrupt
				}
				work.Prepared = &e
			case EntryToolAuthorized:
				if work.Prepared == nil || work.Authorized != nil {
					return RuntimeState{}, nil, ErrCorrupt
				}
				work.Authorized = &e
			case EntryToolStarted:
				if work.Authorized == nil || work.Authorized.Tool.Decision.Block || work.Started != nil {
					return RuntimeState{}, nil, ErrCorrupt
				}
				work.Started = &e
			case EntryToolCompleted:
				if work.Started == nil || work.Completed != nil {
					return RuntimeState{}, nil, ErrCorrupt
				}
				work.Completed = &e
			case EntryToolUnresolved:
				if work.Started == nil || work.Completed != nil || work.Result != nil || work.Control != nil || work.Unresolved != nil || len(state.Branch.PendingCalls) != 1 {
					return RuntimeState{}, nil, fmt.Errorf("%w: invalid unresolved control", ErrCorrupt)
				}
				work.Unresolved = &e
				if state.Branch.Unresolved == nil {
					state.Branch.Unresolved = map[string]ToolRecord{}
				}
				state.Branch.Unresolved[inv.ExecutionID] = *e.Tool
			case EntryToolControl:
				if work.Started == nil || work.Completed != nil || work.Result != nil || work.Control != nil || len(state.Branch.PendingCalls) != 1 {
					return RuntimeState{}, nil, fmt.Errorf("%w: invalid tool control", ErrCorrupt)
				}
				// The control and its completed activation are one durable fact.
				if entryIndex+1 >= len(c.Entries) {
					return RuntimeState{}, nil, fmt.Errorf("%w: missing control termination", ErrCorrupt)
				}
				next := c.Entries[entryIndex+1]
				if next.Kind != EntryRun || next.Run == nil || next.Run.ID != e.RunID || next.Run.Status != "completed" || next.Run.EndReason != e.Tool.Control.Reason {
					return RuntimeState{}, nil, fmt.Errorf("%w: inconsistent control termination", ErrCorrupt)
				}
				work.Control = &e
				work.Unresolved = nil
				delete(state.Branch.PendingCalls, inv.Call.ID)
				if len(state.Branch.PendingCalls) == 0 {
					state.Branch.PendingCalls = nil
				}
			case EntryToolResult, EntryToolResolution:
				if e.Kind == EntryToolResult && work.Completed != nil && work.Completed.Tool.Result.Outcome != e.Tool.Result.Outcome {
					return RuntimeState{}, nil, fmt.Errorf("%w: completed effect changed", ErrCorrupt)
				}
				work.Result = &e
				work.Unresolved = nil
				delete(state.Branch.PendingCalls, inv.Call.ID)
				if len(state.Branch.PendingCalls) == 0 {
					state.Branch.PendingCalls = nil
				}
				if e.Tool.NeedsResolution {
					if state.Branch.Unresolved == nil {
						state.Branch.Unresolved = map[string]ToolRecord{}
					}
					state.Branch.Unresolved[inv.ExecutionID] = *e.Tool
				}
			}
			if e.Kind == EntryToolResult || e.Kind == EntryToolResolution || e.Kind == EntryToolControl || e.Kind == EntryToolUnresolved {
				if state.Branch.ToolEntries == nil {
					state.Branch.ToolEntries = map[string]string{}
				}
				state.Branch.ToolEntries[inv.Call.ID] = e.ID
			}
			state.Tools[inv.Call.ID] = work
		case EntryInputQueued, EntryInputRequeued:
			if state.PendingInputs == nil {
				state.PendingInputs = map[InputKind]uint64{}
			}
			state.PendingInputs[e.Input.Kind]++
		case EntryInputConsumed:
			if state.PendingInputs[e.Input.Kind] == 0 {
				return RuntimeState{}, nil, ErrCorrupt
			}
			state.PendingInputs[e.Input.Kind]--
			if state.PendingInputs[e.Input.Kind] == 0 {
				delete(state.PendingInputs, e.Input.Kind)
				if len(state.PendingInputs) == 0 {
					state.PendingInputs = nil
				}
			}
		case EntryInputCleared:
			if e.Input != nil {
				if state.PendingInputs[e.Input.Kind] == 0 {
					return RuntimeState{}, nil, ErrCorrupt
				}
				state.PendingInputs[e.Input.Kind]--
				if state.PendingInputs[e.Input.Kind] == 0 {
					delete(state.PendingInputs, e.Input.Kind)
				}
			} else {
				for _, kind := range e.Clear.Kinds {
					delete(state.PendingInputs, kind)
				}
			}
			if len(state.PendingInputs) == 0 {
				state.PendingInputs = nil
			}
		case EntryApprovalRequested, EntryApprovalDecided:
			prepared := false
			for _, work := range state.Tools {
				if work.Prepared != nil && work.Prepared.Tool.Invocation.ExecutionID == e.Approval.ExecutionID && work.Authorized == nil && work.Result == nil && work.Control == nil && work.Unresolved == nil {
					prepared = true
					break
				}
			}
			if !prepared {
				return RuntimeState{}, nil, fmt.Errorf("%w: approval outside preparation", ErrCorrupt)
			}
			old, exists := state.Approvals[e.Approval.ExecutionID]
			if e.Kind == EntryApprovalRequested && exists || e.Kind == EntryApprovalDecided && (!exists || old.Decision != nil || old.ID != e.Approval.ID || !sameJSON(old.Request, e.Approval.Request)) {
				return RuntimeState{}, nil, fmt.Errorf("%w: approval decision mismatch", ErrCorrupt)
			}
			if state.Approvals == nil {
				state.Approvals = map[string]ApprovalRecord{}
			}
			state.Approvals[e.Approval.ExecutionID] = *e.Approval
		case EntryCompaction:
			if len(state.Branch.PendingCalls) > 0 || len(state.Branch.Unresolved) > 0 {
				return RuntimeState{}, nil, ErrIncompleteBatch
			}
			state.Branch.CompactionID = e.ID
			state.Branch.ThroughID = e.Compaction.ThroughID
		case EntryRun:
			state.LastRun = clone(e.Run)
			if e.Run.Status == "failed" || e.Run.Status == "cancelled" {
				// An acknowledged stop ends this logical generation. A later
				// Continue may apply queued steering and the current configuration.
				// A process crash has no terminal Run commit and retains the input.
				state.CurrentAttemptID = ""
			}
		}
		r := Record{Entry: e}
		if e.IsHistory() {
			p := clone(state.Branch)
			r.Projection = &p
		}
		records = append(records, r)
	}
	createdAttempt := false
	for _, mutation := range c.Attempts {
		if a := mutation.Create; a != nil {
			if createdAttempt || a.ID != mutation.ID || a.SessionID != state.SessionID || a.Version != previous.Version+1 || a.Input.RolloutID != state.RolloutID {
				return RuntimeState{}, nil, fmt.Errorf("%w: attempt identity or version", ErrCorrupt)
			}
			createdAttempt = true
			if a.Purpose == "generation" {
				state.CurrentAttemptID = a.ID
			}
		}
	}
	state.Version = previous.Version + 1
	return state, records, nil
}

// ForkState validates the immutable boundary. Ancestors are never reduced as
// child commits; its creation entry carries the inherited projection instead.
func ForkState(request ForkRequest, head Record) (RuntimeState, error) {
	if err := ValidateFork(request); err != nil {
		return RuntimeState{}, err
	}
	if head.Projection == nil || head.Entry.ID != request.HeadID {
		return RuntimeState{}, ErrNotFound
	}
	if len(head.Projection.Unresolved) > 0 {
		return RuntimeState{}, ErrOutcomeUnknown
	}
	if !head.Projection.Complete() {
		return RuntimeState{}, ErrIncompleteBatch
	}
	return RuntimeState{FormatVersion: FormatVersion, SessionID: request.TargetID, RolloutID: request.RolloutID(), Branch: clone(*head.Projection)}, nil
}

func ValidateFork(r ForkRequest) error {
	if r.SourceID == "" || r.SourceRolloutID == "" || r.TargetID == "" || r.SourceID == r.TargetID || r.HeadID == "" || r.Mode > ForkDetached {
		return ErrInvalid
	}
	return nil
}

// PrepareFork builds only the child's creation commit, using Store-assigned
// rollout positions. Imported history retains its original writer and version.
func PrepareFork(r ForkRequest, head Record, first uint64) (Commit, RuntimeState, []Record, Receipt, error) {
	if first == 0 || first > math.MaxInt64 {
		return Commit{}, RuntimeState{}, nil, Receipt{}, ErrCorrupt
	}
	seed, err := ForkState(r, head)
	if err != nil {
		return Commit{}, RuntimeState{}, nil, Receipt{}, err
	}
	digest, err := r.Digest()
	if err != nil {
		return Commit{}, RuntimeState{}, nil, Receipt{}, err
	}
	creation := &CreationRecord{ForkDigest: digest, FormatVersion: FormatVersion, SessionID: r.TargetID, RolloutID: seed.RolloutID, ForkRolloutID: r.SourceRolloutID, ForkSessionID: r.SourceID, ForkEntryID: r.HeadID, ForkMode: r.Mode, Projection: &seed.Branch}
	c := Commit{ID: ForkCommitID, Entries: []Entry{{ID: newID(), SessionID: r.TargetID, Version: 1, Time: time.Now().UTC(), Kind: EntryCreated, Created: creation}}}
	for i := range c.Entries {
		c.Entries[i].Sequence = first + uint64(i)
	}
	state, records, err := Reduce(RuntimeState{}, c)
	if err != nil {
		return Commit{}, RuntimeState{}, nil, Receipt{}, err
	}
	receipt := Receipt{CommitID: ForkCommitID, Version: 1, CurrentVersion: 1, Digest: digest, FirstSequence: first, LastSequence: first + uint64(len(c.Entries)) - 1}
	return c, state, records, receipt, err
}
