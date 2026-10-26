package astral

import (
	"fmt"
	"time"
)

// ModelAttempt belongs to a Session's execution history, never its conversation
// tree. Input and identity are immutable. An accepted reply is owned by EntryID;
// only failed or not-yet-adopted outputs live on the attempt itself.
type ModelAttempt struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	RunID     string          `json:"run_id,omitempty"`
	TurnID    string          `json:"turn_id,omitempty"`
	Purpose   string          `json:"purpose"`
	Number    int             `json:"number,omitempty"` // absent for migrated records without a recorded ordinal
	RetryOf   string          `json:"retry_of,omitempty"`
	Version   uint64          `json:"version"`
	StartedAt time.Time       `json:"started_at"`
	Input     RequestInput    `json:"input"`
	Outcome   *AttemptOutcome `json:"outcome,omitempty"`
}

// GenerationProvenance keeps the context-usage anchor self-contained on the
// accepted history node, including when its operational attempts are not cloned.
func GenerationProvenance(attempt ModelAttempt, request Request, reply Message) GenerationRecord {
	covered := clone(request)
	covered.Messages = append(covered.Messages, reply)
	return GenerationRecord{AttemptID: attempt.ID, ConfigurationID: attempt.Input.ConfigurationID, ContextDigest: contextDigest(covered), Model: attempt.Input.Model}
}

type AttemptOutcome struct {
	Status     string    `json:"status"` // completed, failed or interrupted
	FinishedAt time.Time `json:"finished_at"`
	EntryID    string    `json:"entry_id,omitempty"`
	Reply      *Reply    `json:"reply,omitempty"`
	Usage      Usage     `json:"usage,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// AttemptMutation participates in the same CAS/receipt transaction as Entries.
// Create and Finish are each once-only. Adopt moves a completed summary draft
// into its canonical compaction node and removes the temporary output copy.
type AttemptMutation struct {
	ID           string          `json:"id"`
	Create       *ModelAttempt   `json:"create,omitempty"`
	Finish       *AttemptOutcome `json:"finish,omitempty"`
	AdoptEntryID string          `json:"adopt_entry_id,omitempty"`
}

type AttemptQuery struct {
	Cursor   string
	Purpose  string
	Reverse  bool
	Limit    int
	MaxBytes int
}

type AttemptPage struct {
	Attempts []ModelAttempt `json:"attempts"`
	Next     string         `json:"next,omitempty"`
}

// ApplyAttemptMutation is the backend-neutral attempt transition. Adapters call
// it while holding the same transaction used by Reduce and the history append.
func ApplyAttemptMutation(previous *ModelAttempt, mutation AttemptMutation, entries []Entry) (ModelAttempt, error) {
	invalid := fmt.Errorf("%w: invalid model attempt transition", ErrCorrupt)
	if mutation.ID == "" {
		return ModelAttempt{}, invalid
	}
	if mutation.Create != nil {
		a := mutation.Create
		if previous != nil || mutation.Finish != nil || mutation.AdoptEntryID != "" || a.ID != mutation.ID || a.SessionID == "" || a.Version == 0 || a.Number < 1 || a.StartedAt.IsZero() || a.Outcome != nil ||
			(a.Purpose != "generation" && a.Purpose != "compaction") || a.Input.RolloutID == "" || a.Input.HeadID == "" || a.Input.ConfigurationID == "" || a.Input.Model.ID == "" || len(a.Input.Digest) != 64 {
			return ModelAttempt{}, invalid
		}
		return copyJSON(*a)
	}
	if previous == nil || previous.ID != mutation.ID {
		return ModelAttempt{}, invalid
	}
	out := clone(*previous)
	if mutation.AdoptEntryID != "" {
		if mutation.Finish != nil || out.Purpose != "compaction" || out.Outcome == nil || out.Outcome.Status != "completed" || out.Outcome.EntryID != "" || out.Outcome.Reply == nil {
			return ModelAttempt{}, invalid
		}
		for _, entry := range entries {
			if entry.ID == mutation.AdoptEntryID && entry.Compaction != nil && entry.Compaction.AttemptID == out.ID {
				out.Outcome.EntryID, out.Outcome.Reply = entry.ID, nil
				return out, nil
			}
		}
		return ModelAttempt{}, invalid
	}
	result := mutation.Finish
	if result == nil || out.Outcome != nil || result.FinishedAt.IsZero() {
		return ModelAttempt{}, invalid
	}
	switch result.Status {
	case "completed":
		if result.Error != "" {
			return ModelAttempt{}, invalid
		}
		if out.Purpose == "generation" {
			if result.Reply != nil || result.EntryID == "" {
				return ModelAttempt{}, invalid
			}
			matched := false
			for _, entry := range entries {
				if entry.ID == result.EntryID && entry.Generation != nil && entry.Generation.AttemptID == out.ID && entry.Generation.ConfigurationID == out.Input.ConfigurationID && entry.Generation.Model == out.Input.Model && entry.Message != nil && entry.Message.Role == RoleAssistant && !entry.Message.Failed() {
					matched = true
				}
			}
			if !matched {
				return ModelAttempt{}, invalid
			}
		} else if result.Reply == nil || result.EntryID != "" {
			return ModelAttempt{}, invalid
		}
	case "failed", "interrupted":
		if result.EntryID != "" {
			return ModelAttempt{}, invalid
		}
	default:
		return ModelAttempt{}, invalid
	}
	out.Outcome = clone(result)
	return out, nil
}
