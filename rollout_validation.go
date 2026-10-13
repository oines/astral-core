package astral

import (
	"fmt"
)

// ValidateRollout checks envelope and tree integrity. Protocol validity is checked
// separately by context projection: history may stop in the middle of a batch.
func ValidateRollout(r Rollout) error {
	if len(r.Entries) == 0 {
		if r.Version != 0 {
			return fmt.Errorf("%w: nonzero empty version", ErrCorrupt)
		}
		return nil
	}
	if r.Version == 0 || r.Version > uint64(len(r.Entries)) {
		return fmt.Errorf("%w: invalid version", ErrCorrupt)
	}
	ids := make(map[string]Entry, len(r.Entries))
	for n, e := range r.Entries {
		if e.ID == "" {
			return fmt.Errorf("%w: empty entry id", ErrCorrupt)
		}
		if _, ok := ids[e.ID]; ok {
			return fmt.Errorf("%w: duplicate entry %s", ErrCorrupt, e.ID)
		}
		if err := validateEnvelope(e); err != nil {
			return err
		}
		if n == 0 && e.Kind != EntryCreated {
			return fmt.Errorf("%w: missing session header", ErrCorrupt)
		}
		if n > 0 && e.Kind == EntryCreated {
			return fmt.Errorf("%w: duplicate session header", ErrCorrupt)
		}
		if e.IsHistory() {
			if e.ParentID != "" {
				p, ok := ids[e.ParentID]
				if !ok || !p.IsHistory() {
					return fmt.Errorf("%w: missing history parent %s", ErrCorrupt, e.ParentID)
				}
			}
		} else if e.ParentID != "" {
			return fmt.Errorf("%w: operational entry has history parent", ErrCorrupt)
		}
		if e.Kind == EntryHeadSelected {
			p, ok := ids[e.Head.EntryID]
			if !ok || !p.IsHistory() {
				return fmt.Errorf("%w: selected head missing", ErrCorrupt)
			}
		}
		if e.Kind == EntryCompaction {
			found := false
			for id := e.ParentID; id != ""; {
				p := ids[id]
				if id == e.Compaction.ThroughID {
					found = true
					break
				}
				id = p.ParentID
			}
			if !found {
				return fmt.Errorf("%w: compaction boundary is not an ancestor", ErrCorrupt)
			}
		}
		ids[e.ID] = e
	}
	return nil
}

func validateEnvelope(e Entry) error {
	n := 0
	for _, v := range []bool{e.Created != nil, e.Configuration != nil, e.Environment != nil, e.Message != nil, e.Tool != nil, e.Approval != nil, e.Input != nil, e.Head != nil, e.Compaction != nil, e.Attempt != nil, e.Run != nil} {
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
		valid = e.Created != nil && e.Created.FormatVersion == 1 && e.Created.SessionID != ""
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
	case EntryAttempt:
		valid = e.Attempt != nil && e.Attempt.ID != "" && e.Attempt.ModelID != "" && (e.Attempt.Status == "started" || e.Attempt.Status == "succeeded" || e.Attempt.Status == "failed")
	case EntryRun:
		valid = e.Run != nil && e.Run.ID != "" && (e.Run.Status == "started" || e.Run.Status == "resumed" || e.Run.Status == "completed" || e.Run.Status == "failed" || e.Run.Status == "cancelled")
	}
	if !valid {
		return fmt.Errorf("%w: invalid %s payload in %s", ErrCorrupt, e.Kind, e.ID)
	}
	return nil
}
