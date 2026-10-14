package session

import (
	"fmt"

	protocol "github.com/oines/astral-core"
)

// Projection contains branch-local references and unfinished protocol work.
// It never embeds completed conversation history or a queue backlog.
type Projection struct {
	HeadID               string                       `json:"head_id,omitempty"`
	ConfigurationID      string                       `json:"configuration_id,omitempty"`
	EnvironmentID        string                       `json:"environment_id,omitempty"`
	CompactionID         string                       `json:"compaction_id,omitempty"`
	ThroughID            string                       `json:"through_id,omitempty"`
	LastMessageID        string                       `json:"last_message_id,omitempty"`
	BatchConfigurationID string                       `json:"batch_configuration_id,omitempty"`
	PendingCalls         map[string]protocol.ToolCall `json:"pending_calls,omitempty"`
	Unresolved           map[string]ToolRecord        `json:"unresolved,omitempty"`
}

func (p Projection) Complete() bool { return len(p.PendingCalls) == 0 }

// ToolState retains only the current assistant batch's execution facts.
// Exact runtime checkpoints are distinct from model-generated summaries.
type ToolState struct {
	Prepared   *Entry `json:"prepared,omitempty"`
	Authorized *Entry `json:"authorized,omitempty"`
	Started    *Entry `json:"started,omitempty"`
	Completed  *Entry `json:"completed,omitempty"`
	Result     *Entry `json:"result,omitempty"`
}
type RuntimeState struct {
	FormatVersion    int                       `json:"format_version"`
	SessionID        string                    `json:"session_id"`
	Version          uint64                    `json:"version"`
	Sequence         uint64                    `json:"sequence"`
	Creation         CreationRecord            `json:"creation"`
	Branch           Projection                `json:"branch"`
	Tools            map[string]ToolState      `json:"tools,omitempty"`
	Approvals        map[string]ApprovalRecord `json:"approvals,omitempty"`
	PendingInputs    map[InputKind]uint64      `json:"pending_inputs,omitempty"`
	BoundaryRequired bool                      `json:"boundary_required,omitempty"`
}

// Reduce is the single state transition used by live commits, backend replay,
// and checkpoint verification. Its cost depends on this batch and unfinished
// work, never on the total number of historical records.
func Reduce(previous RuntimeState, c Commit) (RuntimeState, []Record, error) {
	if c.ID == "" || len(c.Entries) == 0 || c.ExpectedVersion != previous.Version {
		return RuntimeState{}, nil, ErrConflict
	}
	if previous.Version > 0 && previous.FormatVersion != FormatVersion {
		return RuntimeState{}, nil, ErrUnsupportedFormat
	}
	state, err := copyJSON(previous)
	if err != nil {
		return RuntimeState{}, nil, err
	}
	records := make([]Record, 0, len(c.Entries))
	ids := map[string]bool{}
	for _, entry := range c.Entries {
		e, err := copyJSON(entry)
		if err != nil {
			return RuntimeState{}, nil, err
		}
		if err = validateEnvelope(e); err != nil {
			return RuntimeState{}, nil, err
		}
		if e.ID == "" || ids[e.ID] || e.Sequence != state.Sequence+1 || e.Version != previous.Version+1 {
			return RuntimeState{}, nil, fmt.Errorf("%w: entry identity, sequence or version", ErrCorrupt)
		}
		ids[e.ID] = true
		state.Sequence = e.Sequence
		if previous.Version == 0 && len(records) == 0 {
			if e.Kind != EntryCreated || e.Created.FormatVersion != FormatVersion {
				return RuntimeState{}, nil, ErrUnsupportedFormat
			}
			state.FormatVersion = FormatVersion
			state.SessionID = e.Created.SessionID
			state.Creation = *e.Created
			state.BoundaryRequired = e.Created.ForkSessionID != ""
		} else if e.Kind == EntryCreated {
			return RuntimeState{}, nil, fmt.Errorf("%w: duplicate creation", ErrCorrupt)
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
		case EntryEnvironment:
			state.Branch.EnvironmentID = e.ID
		case EntryMessage:
			if len(state.Branch.PendingCalls) > 0 {
				return RuntimeState{}, nil, protocol.ErrIncompleteBatch
			}
			if len(state.Branch.Unresolved) > 0 {
				return RuntimeState{}, nil, protocol.ErrOutcomeUnknown
			}
			state.Branch.LastMessageID = e.ID
			state.Branch.BatchConfigurationID = state.Branch.ConfigurationID
			state.Branch.PendingCalls = nil
			state.Tools = nil
			state.Approvals = nil
			if len(e.Message.ToolCalls) > 0 {
				state.Branch.PendingCalls = map[string]protocol.ToolCall{}
				for _, call := range e.Message.ToolCalls {
					state.Branch.PendingCalls[call.ID] = call
				}
			}
		case EntryToolPrepared, EntryToolAuthorized, EntryToolStarted, EntryToolCompleted, EntryToolResult, EntryToolResolution:
			inv := e.Tool.Invocation
			if e.Kind == EntryToolResolution {
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
			case EntryToolResult, EntryToolResolution:
				if e.Kind == EntryToolResult && work.Completed != nil && (work.Completed.Tool.Result.Outcome != e.Tool.Result.Outcome || !sameJSON(work.Completed.Tool.Result.Context, e.Tool.Result.Context)) {
					return RuntimeState{}, nil, fmt.Errorf("%w: completed effect changed", ErrCorrupt)
				}
				work.Result = &e
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
			state.Tools[inv.Call.ID] = work
		case EntryInputQueued:
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
		case EntryApprovalRequested, EntryApprovalDecided:
			prepared := false
			for _, work := range state.Tools {
				if work.Prepared != nil && work.Prepared.Tool.Invocation.ExecutionID == e.Approval.ExecutionID && work.Authorized == nil && work.Result == nil {
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
		case EntryHeadSelected:
			if c.Selection == nil || c.Selection.HeadID != e.Head.EntryID {
				return RuntimeState{}, nil, ErrCorrupt
			}
			state.Branch = clone(*c.Selection)
			state.Tools = nil
			state.Approvals = nil
			state.BoundaryRequired = true
		case EntryCompaction:
			if len(state.Branch.PendingCalls) > 0 || len(state.Branch.Unresolved) > 0 {
				return RuntimeState{}, nil, protocol.ErrIncompleteBatch
			}
			state.Branch.CompactionID = e.ID
			state.Branch.ThroughID = e.Compaction.ThroughID
		case EntryRun:
			if e.Run.Status == "started" || e.Run.Status == "resumed" {
				state.BoundaryRequired = false
			}
		}
		r := Record{Entry: e}
		if e.IsHistory() {
			p := clone(state.Branch)
			r.Projection = &p
		}
		records = append(records, r)
	}
	state.Version = previous.Version + 1
	return state, records, nil
}

// ForkState resets operational work while inheriting the chosen history only.
func ForkState(request ForkRequest, head Record) (RuntimeState, error) {
	if err := ValidateFork(request); err != nil {
		return RuntimeState{}, err
	}
	if head.Projection == nil || head.Entry.Version > request.SourceVersion {
		return RuntimeState{}, ErrNotFound
	}
	if !head.Projection.Complete() && !request.AllowIncomplete {
		return RuntimeState{}, protocol.ErrIncompleteBatch
	}
	state := RuntimeState{FormatVersion: FormatVersion, SessionID: request.TargetID, Version: 1, Sequence: head.Entry.Sequence, Creation: *request.Creation.Created, Branch: clone(*head.Projection), BoundaryRequired: true}
	return state, nil
}

func ValidateFork(r ForkRequest) error {
	if r.SourceID == "" || r.TargetID == "" || r.SourceID == r.TargetID || r.HeadID == "" || r.SourceVersion == 0 || r.Creation.Kind != EntryCreated || r.Creation.Created == nil || r.Creation.Created.SessionID != r.TargetID || r.Creation.Created.FormatVersion != FormatVersion || r.Creation.ID == "" || r.Creation.Sequence != 1 || r.Creation.Version != 1 {
		return protocol.ErrInvalid
	}
	return nil
}
