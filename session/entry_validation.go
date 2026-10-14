package session

import (
	"fmt"
)

func validateEnvelope(e Entry) error {
	n := 0
	for _, v := range []bool{e.Created != nil, e.Configuration != nil, e.Environment != nil, e.Message != nil, e.Tool != nil, e.Approval != nil, e.Input != nil, e.Head != nil, e.Compaction != nil, e.Run != nil} {
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
		valid = e.Created != nil && e.Created.FormatVersion == FormatVersion && e.Created.SessionID != ""
	case EntryConfiguration:
		valid = e.Configuration != nil && e.Configuration.ModelID != ""
	case EntryEnvironment:
		valid = e.Environment != nil
	case EntryMessage:
		valid = e.Message != nil
		if valid {
			if err := validateMessage(*e.Message); err != nil {
				return fmt.Errorf("%w: %v", ErrCorrupt, err)
			}
		}
	case EntryToolPrepared, EntryToolAuthorized, EntryToolStarted, EntryToolCompleted, EntryToolResult, EntryToolResolution:
		valid = e.Tool != nil && e.Tool.Invocation.ExecutionID != "" && e.Tool.Invocation.AssistantID != "" && e.Tool.Invocation.Call.ID != ""
		if valid && (e.Kind == EntryToolResult || e.Kind == EntryToolResolution || e.Kind == EntryToolCompleted) {
			valid = e.Tool.Result != nil && validOutcome(e.Tool.Result.Outcome)
		}
		if valid && e.Kind == EntryToolAuthorized {
			valid = e.Tool.Decision != nil
		}
	case EntryApprovalRequested, EntryApprovalDecided:
		valid = e.Approval != nil && e.Approval.ID != "" && e.Approval.ExecutionID != ""
		if valid && e.Kind == EntryApprovalDecided {
			valid = e.Approval.Decision != nil
		}
	case EntryInputQueued, EntryInputConsumed:
		valid = e.Input != nil && e.Input.ID != "" && (e.Input.Kind == InputSteer || e.Input.Kind == InputFollowUp)
		if valid && e.Kind == EntryInputQueued {
			valid = e.Input.Message != nil
			if valid {
				valid = validateUser(*e.Input.Message) == nil
			}
		}
	case EntryHeadSelected:
		valid = e.Head != nil && e.Head.EntryID != ""
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
