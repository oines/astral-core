package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	core "github.com/oines/astral-core"
)

// These are frozen format-11 provenance fields, independent of future Core
// models, message structs and projection code. No message body is interpreted.
type generationAttemptV11 struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	TurnID    string `json:"turn_id"`
	Version   uint64 `json:"version"`
	Purpose   string `json:"purpose"`
	Input     struct {
		RolloutID       string             `json:"rollout_id"`
		HeadID          string             `json:"head_id"`
		ConfigurationID string             `json:"configuration_id"`
		Selection       *[]int             `json:"selection"`
		Changes         []requestChangeV11 `json:"changes"`
	} `json:"input"`
}

type requestChangeV11 struct {
	Path  []string        `json:"path"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`
}

type sourceEntryV11 struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Sequence uint64 `json:"sequence"`
	Kind     string `json:"kind"`
	Message  *struct {
		Role         string `json:"role"`
		FinishReason string `json:"finish_reason"`
		Content      []struct {
			Type     string `json:"type"`
			ToolCall *struct {
				ID string `json:"id"`
			} `json:"tool_call"`
		} `json:"content"`
	} `json:"message"`
	Tool *struct {
		Invocation struct {
			AssistantID string `json:"assistant_id"`
			Call        struct {
				ID string `json:"id"`
			} `json:"call"`
		} `json:"invocation"`
	} `json:"tool"`
}

// migrateInputGeneration is an offline, transaction-scoped format conversion.
// Only a frozen generation boundary can own an input. Standalone compaction and
// passive application do not. Message bodies, attempts and receipts stay intact.
func migrateInputGeneration(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
ALTER TABLE inputs ADD COLUMN accepted_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE inputs ADD COLUMN entry_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE inputs ADD COLUMN generation BLOB;
CREATE INDEX input_uncovered ON inputs(session_id,entry_seq) WHERE generation IS NULL;
UPDATE inputs SET entry_seq=COALESCE((SELECT e.seq FROM entries e JOIN sessions s ON s.rollout_id=e.rollout_id WHERE s.id=inputs.session_id AND e.id=inputs.entry_id),0);
UPDATE inputs SET accepted_run_id=COALESCE((SELECT json_extract(e.payload,'$.run_id') FROM entries e JOIN sessions s ON s.rollout_id=e.rollout_id WHERE s.id=inputs.session_id AND e.session_id=inputs.session_id AND e.kind='input_queued' AND e.reference=inputs.id ORDER BY e.seq LIMIT 1),'');
WITH runs AS (
 SELECT session_id,rollout_id,seq,json_extract(payload,'$.run.id') AS run_id,json_extract(payload,'$.run.status') AS status,
 LEAD(seq) OVER(PARTITION BY session_id ORDER BY seq) AS next_seq,
 LEAD(json_extract(payload,'$.run.id')) OVER(PARTITION BY session_id ORDER BY seq) AS next_run_id,
 LEAD(json_extract(payload,'$.run.status')) OVER(PARTITION BY session_id ORDER BY seq) AS next_status
 FROM entries WHERE kind='run'
), intervals AS (
 SELECT * FROM runs WHERE status='started' AND next_run_id=run_id AND next_status IN ('completed','failed','cancelled')
)
UPDATE inputs SET accepted_run_id=COALESCE((
 SELECT i.run_id FROM entries q JOIN sessions s ON s.id=inputs.session_id AND s.rollout_id=q.rollout_id
 JOIN intervals i ON i.session_id=q.session_id AND i.rollout_id=q.rollout_id AND i.seq<q.seq AND q.seq<i.next_seq
 WHERE q.session_id=inputs.session_id AND q.kind='input_queued' AND q.reference=inputs.id ORDER BY q.seq LIMIT 1
),'') WHERE accepted_run_id='';
UPDATE sessions SET state=json_set(state,'$.format_version',12);
`)
	if err != nil {
		return err
	}
	sessionID, version := "", uint64(0)
	for {
		rows, err := tx.QueryContext(ctx, `SELECT a.session_id,a.id,a.version,a.rollout_id,s.rollout_id,a.payload FROM attempts a JOIN sessions s ON s.id=a.session_id JOIN rollouts r ON r.id=a.rollout_id WHERE a.purpose='generation' AND s.ready=1 AND r.status='active' AND (a.session_id>? OR a.session_id=? AND a.version>?) ORDER BY a.session_id,a.version LIMIT 64`, sessionID, sessionID, version)
		if err != nil {
			return err
		}
		var attempts []generationAttemptV11
		for rows.Next() {
			var raw []byte
			var indexedSession, indexedID, indexedRollout, sessionRollout string
			var indexedVersion uint64
			var attempt generationAttemptV11
			if err = rows.Scan(&indexedSession, &indexedID, &indexedVersion, &indexedRollout, &sessionRollout, &raw); err != nil {
				break
			}
			if json.Unmarshal(raw, &attempt) != nil || attempt.ID == "" || attempt.SessionID == "" || attempt.Version == 0 || attempt.ID != indexedID || attempt.SessionID != indexedSession || attempt.Version != indexedVersion || attempt.Input.RolloutID != indexedRollout || indexedRollout != sessionRollout || attempt.Purpose != "generation" {
				err = core.ErrCorrupt
				break
			}
			attempts = append(attempts, attempt)
		}
		if rowsErr := rows.Err(); err == nil {
			err = rowsErr
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(attempts) == 0 {
			return nil
		}
		for _, attempt := range attempts {
			input := generationSourcesV12{SessionID: attempt.SessionID, RolloutID: attempt.Input.RolloutID, HeadID: attempt.Input.HeadID, Owner: inputGenerationV12{AttemptID: attempt.ID, RunID: attempt.RunID, TurnID: attempt.TurnID}}
			var sequence uint64
			var projection []byte
			if err = tx.QueryRowContext(ctx, "SELECT seq,projection FROM entries WHERE rollout_id=? AND id=? AND projection IS NOT NULL", input.RolloutID, input.HeadID).Scan(&sequence, &projection); err != nil {
				return core.ErrCorrupt
			}
			var head struct {
				HeadID          string                     `json:"head_id"`
				ConfigurationID string                     `json:"configuration_id"`
				PendingCalls    map[string]json.RawMessage `json:"pending_calls"`
				Unresolved      map[string]json.RawMessage `json:"unresolved"`
			}
			if json.Unmarshal(projection, &head) != nil || head.HeadID != input.HeadID || head.ConfigurationID != attempt.Input.ConfigurationID || len(head.PendingCalls)+len(head.Unresolved) > 0 {
				return core.ErrCorrupt
			}
			changed := false
			for _, change := range attempt.Input.Changes {
				changed = changed || len(change.Path) == 0 || change.Path[0] == "messages"
			}
			// Identity selection plus no message edits proves the entire ordinary
			// projection survived. Its visible nodes and summary cover the prefix.
			if attempt.Input.Selection == nil && !changed {
				owner, _ := json.Marshal(input.Owner)
				if _, err = tx.ExecContext(ctx, "UPDATE inputs SET generation=? WHERE session_id=? AND entry_seq>0 AND entry_seq<=? AND generation IS NULL", owner, input.SessionID, sequence); err != nil {
					return err
				}
				sessionID, version = attempt.SessionID, attempt.Version
				continue
			}
			sources, err := generationSourceIDsV11(ctx, tx, attempt.Input.RolloutID, attempt.Input.HeadID)
			if err != nil {
				return fmt.Errorf("generation provenance %s: %w", attempt.ID, err)
			}
			if changed {
				sources, err = provenRecipeSourcesV11(ctx, tx, attempt, sources)
				if err != nil {
					return err
				}
			} else if selection := attempt.Input.Selection; selection != nil {
				selected := make([]string, len(*selection))
				for i, at := range *selection {
					if at < -1 || at >= len(sources) {
						return core.ErrCorrupt
					}
					if at >= 0 {
						selected[i] = sources[at]
					}
				}
				sources = selected
			}
			input.Sources = sources
			if err = saveInputGenerationV12(ctx, tx, input); err != nil {
				return err
			}
			sessionID, version = attempt.SessionID, attempt.Version
		}
	}
}

// generationSourceIDsV11 reproduces only format 11's source ordering: summary,
// then visible messages, with results placed in each assistant's call order.
// Failed assistants and control-only batches were absent from model input.
func generationSourceIDsV11(ctx context.Context, tx *sql.Tx, rolloutID, headID string) ([]string, error) {
	var projection []byte
	if err := tx.QueryRowContext(ctx, "SELECT projection FROM entries WHERE rollout_id=? AND id=?", rolloutID, headID).Scan(&projection); err != nil {
		return nil, err
	}
	var boundary struct {
		HeadID       string `json:"head_id"`
		CompactionID string `json:"compaction_id"`
		ThroughID    string `json:"through_id"`
	}
	if json.Unmarshal(projection, &boundary) != nil || boundary.HeadID != headID {
		return nil, core.ErrCorrupt
	}
	var path []sourceEntryV11
	seen := map[string]bool{}
	for id := headID; id != "" && id != boundary.ThroughID; {
		if seen[id] {
			return nil, core.ErrCorrupt
		}
		seen[id] = true
		var payload []byte
		var entry sourceEntryV11
		var parent, kind string
		var sequence uint64
		if err := tx.QueryRowContext(ctx, "SELECT parent_id,kind,seq,payload FROM entries WHERE rollout_id=? AND id=? AND projection IS NOT NULL", rolloutID, id).Scan(&parent, &kind, &sequence, &payload); err != nil {
			return nil, err
		}
		if json.Unmarshal(payload, &entry) != nil || entry.ID != id || entry.ParentID != parent || entry.Kind != kind || entry.Sequence != sequence {
			return nil, core.ErrCorrupt
		}
		path = append(path, entry)
		id = entry.ParentID
		if id == "" && boundary.ThroughID != "" {
			return nil, core.ErrCorrupt
		}
	}
	sort.Slice(path, func(i, j int) bool { return path[i].Sequence < path[j].Sequence })
	results, controls := map[string]string{}, map[string]bool{}
	for _, entry := range path {
		if entry.Kind == "tool_result" || entry.Kind == "tool_resolution" || entry.Kind == "tool_control" {
			if entry.Tool == nil {
				return nil, core.ErrCorrupt
			}
			inv := entry.Tool.Invocation
			results[inv.AssistantID+"\x00"+inv.Call.ID] = entry.ID
			controls[inv.AssistantID] = controls[inv.AssistantID] || entry.Kind == "tool_control"
		}
	}
	var sources []string
	if boundary.CompactionID != "" {
		sources = append(sources, boundary.CompactionID)
	}
	for _, entry := range path {
		switch entry.Kind {
		case "context":
			sources = append(sources, entry.ID)
		case "message":
			message := entry.Message
			if message == nil {
				return nil, core.ErrCorrupt
			}
			if controls[entry.ID] || message.Role == "assistant" && (message.FinishReason == "error" || message.FinishReason == "cancelled") {
				continue
			}
			sources = append(sources, entry.ID)
			for _, part := range message.Content {
				if part.Type == "tool_call" && part.ToolCall != nil {
					result := results[entry.ID+"\x00"+part.ToolCall.ID]
					if result == "" {
						return nil, core.ErrCorrupt
					}
					sources = append(sources, result)
				}
			}
		}
	}
	return sources, nil
}
