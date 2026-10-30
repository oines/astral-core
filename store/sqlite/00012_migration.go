package sqlite

// Frozen schema 11 -> 12 conversion. It reads envelopes with RawMessage fields,
// not the current Core reducer or domain types. Unknown fields and exact opaque
// JSON values survive. The 00012_codec grammar is part of this migration format.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
)

func sameJSON12(a, b []byte) bool {
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	return da.Decode(&x) == nil && db.Decode(&y) == nil && reflect.DeepEqual(x, y)
}

func convert12(ctx context.Context, tx *sql.Tx, rollout, kind, key string, raw []byte, shape *field12) ([]byte, error) {
	encoded, blobs, err := encode12(raw, shape)
	if err != nil {
		return nil, err
	}
	var pack packed12
	if err = json.Unmarshal(encoded, &pack); err != nil {
		return nil, err
	}
	restored, err := decode12(pack, shape, blobs)
	if err != nil || !sameJSON12(raw, restored) {
		return nil, fmt.Errorf("schema 12: %s %s failed round-trip validation", kind, key)
	}
	if err = replacePayloadRefs(ctx, tx, rollout, kind, key, blobs); err != nil {
		return nil, err
	}
	return encoded, nil
}

func migratePayloads12(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, payloadSchema12+`
ALTER TABLE entries ADD COLUMN payload_size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE entries ADD COLUMN projection_size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attempts ADD COLUMN payload_size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE inputs ADD COLUMN current_queue_entry_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pending ADD COLUMN entry_id TEXT NOT NULL DEFAULT '';
`); err != nil {
		return err
	}
	if err := payloadCandidates(ctx, tx); err != nil {
		return err
	}
	// One row at a time bounds memory by the largest existing record. The stable
	// rowid cursor is unaffected by updates and every read is closed before write.
	var cursor int64
	for {
		var row int64
		var rollout, id string
		var raw, projection []byte
		err := tx.QueryRowContext(ctx, `SELECT rowid,rollout_id,id,payload,projection FROM entries WHERE rowid>? ORDER BY rowid LIMIT 1`, cursor).Scan(&row, &rollout, &id, &raw, &projection)
		if err == sql.ErrNoRows {
			break
		}
		if err != nil {
			return err
		}
		encoded, blobs, err := encode12(raw, entry12)
		if err != nil {
			return err
		}
		var p any
		if len(projection) > 0 {
			packed, more, err := encode12(projection, projection12)
			if err != nil {
				return err
			}
			p = packed
			for h, b := range more {
				blobs[h] = b
			}
		}
		for _, item := range []struct {
			old, encoded []byte
			shape        *field12
		}{{raw, encoded, entry12}, {projection, asBytes12(p), projection12}} {
			if len(item.old) == 0 {
				continue
			}
			var pack packed12
			if json.Unmarshal(item.encoded, &pack) != nil {
				return fmt.Errorf("schema 12: invalid encoding")
			}
			decoded, err := decode12(pack, item.shape, blobs)
			if err != nil || !sameJSON12(item.old, decoded) {
				return fmt.Errorf("schema 12: entry %s failed round-trip validation", id)
			}
		}
		if err = replacePayloadRefs(ctx, tx, rollout, "entry", ownerKey(rollout, id), blobs); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE entries SET payload=?,projection=?,payload_size=?,projection_size=? WHERE rowid=?`, encoded, p, len(raw), len(projection), row); err != nil {
			return err
		}
		cursor = row
	}
	for _, kind := range []string{"state", "attempt"} {
		cursor = 0
		table, column, identity := "sessions", "state", "id"
		shape := state12
		if kind == "attempt" {
			table, column, identity = "attempts", "payload", "session_id"
			shape = attempt12
		}
		for {
			var row int64
			var rollout, session, id string
			var raw []byte
			err := tx.QueryRowContext(ctx, `SELECT rowid,rollout_id,`+identity+`,id,`+column+` FROM `+table+` WHERE rowid>? ORDER BY rowid LIMIT 1`, cursor).Scan(&row, &rollout, &session, &id, &raw)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return err
			}
			key := ownerKey(session)
			if kind == "attempt" {
				key = ownerKey(session, id)
			}
			encoded, err := convert12(ctx, tx, rollout, kind, key, raw, shape)
			if err != nil {
				return err
			}
			query := `UPDATE ` + table + ` SET ` + column + `=?`
			args := []any{encoded}
			if kind == "attempt" {
				query += `,payload_size=?`
				args = append(args, len(raw))
			}
			query += ` WHERE rowid=?`
			args = append(args, row)
			if _, err = tx.ExecContext(ctx, query, args...); err != nil {
				return err
			}
			cursor = row
		}
	}
	// Resolve the current queue revision, including cleared/applied receipts.
	// The original acceptance digest deliberately does not identify its content.
	cursor = 0
	for {
		var row int64
		var session, id, digest, kind, rollout string
		var old []byte
		err := tx.QueryRowContext(ctx, `SELECT i.rowid,i.session_id,i.id,i.digest,i.kind,i.payload,s.rollout_id FROM inputs i LEFT JOIN sessions s ON s.id=i.session_id WHERE i.rowid>? ORDER BY i.rowid LIMIT 1`, cursor).Scan(&row, &session, &id, &digest, &kind, &old, &rollout)
		if err == sql.ErrNoRows {
			break
		}
		if err != nil {
			return err
		}
		var eventID string
		var packed []byte
		err = tx.QueryRowContext(ctx, `SELECT id,payload FROM entries WHERE rollout_id=? AND session_id=? AND reference=? AND kind IN ('input_queued','input_requeued') ORDER BY seq DESC LIMIT 1`, rollout, session, id).Scan(&eventID, &packed)
		if err != nil {
			return fmt.Errorf("schema 12: input %s has no queue revision: %w", id, err)
		}
		raw, err := hydrate(ctx, tx, packedValue{packed, entry12})
		if err != nil {
			return err
		}
		var event struct {
			Input json.RawMessage `json:"input"`
		}
		var input struct{ ID, Kind, Digest string }
		if json.Unmarshal(raw[0], &event) != nil || json.Unmarshal(event.Input, &input) != nil || input.ID != id || input.Kind != kind || input.Digest != digest || !sameJSON12(old, event.Input) {
			return fmt.Errorf("schema 12: input %s does not match its latest queue revision", id)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE inputs SET current_queue_entry_id=? WHERE rowid=?`, eventID, row); err != nil {
			return err
		}
		cursor = row
	}
	cursor = 0
	for {
		var row, seq int64
		var rollout, session string
		var old []byte
		err := tx.QueryRowContext(ctx, `SELECT rowid,rollout_id,session_id,seq,payload FROM pending WHERE rowid>? ORDER BY rowid LIMIT 1`, cursor).Scan(&row, &rollout, &session, &seq, &old)
		if err == sql.ErrNoRows {
			break
		}
		if err != nil {
			return err
		}
		var event struct {
			ID       string `json:"id"`
			Session  string `json:"session_id"`
			Sequence int64  `json:"sequence"`
		}
		if json.Unmarshal(old, &event) != nil || event.ID == "" || event.Session != session || event.Sequence != seq {
			return fmt.Errorf("schema 12: invalid pending event")
		}
		var packed []byte
		if err = tx.QueryRowContext(ctx, `SELECT payload FROM entries WHERE rollout_id=? AND id=? AND session_id=? AND seq=?`, rollout, event.ID, session, seq).Scan(&packed); err != nil {
			return err
		}
		raw, err := hydrate(ctx, tx, packedValue{packed, entry12})
		if err != nil {
			return err
		}
		if !sameJSON12(old, raw[0]) {
			return fmt.Errorf("schema 12: pending event %s disagrees with history", event.ID)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE pending SET entry_id=? WHERE rowid=?`, event.ID, row); err != nil {
			return err
		}
		cursor = row
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE inputs DROP COLUMN payload; ALTER TABLE pending DROP COLUMN payload;`); err != nil {
		return err
	}
	return collectPayloads(ctx, tx)
}

func asBytes12(value any) []byte { raw, _ := value.([]byte); return raw }
