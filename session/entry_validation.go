package session

import (
	"fmt"
	protocol "github.com/oines/astral-core"
)

func validateEnvelope(e Entry) error {
	n := 0
	for _, v := range []bool{e.Created != nil, e.Configuration != nil, e.Message != nil, e.Tool != nil, e.Approval != nil, e.Input != nil, e.Compaction != nil, e.Run != nil, e.Clear != nil, e.Turn != nil} {
		if v {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("%w: %s needs exactly one payload", ErrCorrupt, e.ID)
	}
	valid := false
	switch e.Kind {
	case EntryCreated:
		valid = e.Created != nil && e.Created.FormatVersion == FormatVersion && e.Created.SessionID != "" && e.Created.RolloutID != ""
	case EntryConfiguration:
		valid = e.Configuration != nil && e.Configuration.ModelID != ""
	case EntryContext:
		valid = e.Message != nil && e.Message.ValidateContext() == nil
	case EntryMessage:
		valid = e.Message != nil
		if valid {
			if err := validateMessage(*e.Message); err != nil {
				return fmt.Errorf("%w: %v", ErrCorrupt, err)
			}
		}
	case EntryToolPrepared, EntryToolAuthorized, EntryToolStarted, EntryToolCompleted, EntryToolResult, EntryToolResolution, EntryToolControl, EntryToolUnresolved:
		valid = e.Tool != nil && e.Tool.Invocation.ExecutionID != "" && e.Tool.Invocation.AssistantID != "" && e.Tool.Invocation.Call.ID != ""
		if valid && (e.Kind == EntryToolResult || e.Kind == EntryToolResolution || e.Kind == EntryToolCompleted) {
			valid = e.Tool.Result != nil && validOutcome(e.Tool.Result.Outcome)
		}
		if valid && e.Kind == EntryToolAuthorized {
			valid = e.Tool.Decision != nil
		}
		if valid && e.Kind == EntryToolControl {
			valid = e.Tool.Control != nil && e.Tool.Result == nil && !e.Tool.NeedsResolution
		} else if valid {
			valid = e.Tool.Control == nil
		}
		if valid && e.Kind == EntryToolUnresolved {
			valid = e.Tool.Result == nil && e.Tool.NeedsResolution
		}
	case EntryApprovalRequested, EntryApprovalDecided:
		valid = e.Approval != nil && e.Approval.ID != "" && e.Approval.ExecutionID != ""
		if valid && e.Kind == EntryApprovalDecided {
			valid = e.Approval.Decision != nil
		}
	case EntryInputQueued, EntryInputConsumed:
		valid = e.Input != nil && e.Input.ID != "" && (e.Input.Kind == InputSteer || e.Input.Kind == InputFollowUp || e.Input.Kind == InputContext)
		if valid && e.Kind == EntryInputQueued {
			valid = e.Input.Validate() == nil
		}
	case EntryInputCleared:
		valid = e.Clear != nil && len(e.Clear.Kinds) > 0
		if valid {
			for _, k := range e.Clear.Kinds {
				valid = valid && (k == InputSteer || k == InputFollowUp)
			}
		}
	case EntryTurn:
		valid = e.Turn != nil && e.Turn.ID != "" && e.Turn.AssistantID != "" && (e.Turn.Decision == protocol.TurnDefault || e.Turn.Decision == protocol.TurnEnd || e.Turn.Decision == protocol.TurnContinue)
	case EntryCompaction:
		valid = e.Compaction != nil && e.Compaction.ThroughID != "" && e.Compaction.Summary != ""
	case EntryRun:
		valid = e.Run != nil && e.Run.ID != "" && (e.Run.Status == "started" || e.Run.Status == "resumed" || e.Run.Status == "completed" || e.Run.Status == "failed" || e.Run.Status == "cancelled")
	}
	if !valid {
		return fmt.Errorf("%w: invalid %s payload in %s", ErrCorrupt, e.Kind, e.ID)
	}
	return nil
}
