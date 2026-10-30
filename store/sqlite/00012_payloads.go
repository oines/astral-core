package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	core "github.com/oines/astral-core"
)

const payloadSchema12 = `
CREATE TABLE payloads(hash TEXT PRIMARY KEY,byte_length INTEGER NOT NULL,bytes BLOB NOT NULL);
CREATE TABLE payload_refs(rollout_id TEXT NOT NULL,owner_kind TEXT NOT NULL,owner_key TEXT NOT NULL,hash TEXT NOT NULL REFERENCES payloads(hash),PRIMARY KEY(owner_kind,owner_key,hash));
CREATE INDEX payload_ref_hash ON payload_refs(hash);
CREATE INDEX payload_ref_rollout ON payload_refs(rollout_id);
`

func ownerKey(parts ...string) string { raw, _ := json.Marshal(parts); return string(raw) }

// Candidates live in a connection-local temporary table, not a Go slice of the
// whole rollout. All references are installed before collecting candidates.
func payloadCandidates(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS payload_candidates(hash TEXT PRIMARY KEY); DELETE FROM payload_candidates;`)
	return err
}

func replacePayloadRefs(ctx context.Context, tx *sql.Tx, rollout, kind, key string, blobs map[string][]byte) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO payload_candidates SELECT hash FROM payload_refs WHERE owner_kind=? AND owner_key=?`, kind, key); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM payload_refs WHERE owner_kind=? AND owner_key=?`, kind, key); err != nil {
		return err
	}
	for hash, raw := range blobs {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO payloads(hash,byte_length,bytes) VALUES(?,?,?)`, hash, len(raw), raw)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var length int
			var stored []byte
			if err = tx.QueryRowContext(ctx, `SELECT byte_length,bytes FROM payloads WHERE hash=?`, hash).Scan(&length, &stored); err != nil {
				return err
			}
			if length != len(raw) || !bytes.Equal(raw, stored) {
				return fmt.Errorf("%w: payload collision or corruption", core.ErrCorrupt)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO payload_refs(rollout_id,owner_kind,owner_key,hash) VALUES(?,?,?,?)`, rollout, kind, key, hash); err != nil {
			return err
		}
	}
	return nil
}

func collectPayloads(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM payloads WHERE hash IN (SELECT hash FROM payload_candidates) AND NOT EXISTS(SELECT 1 FROM payload_refs r WHERE r.hash=payloads.hash); DELETE FROM payload_candidates;`)
	return err
}

func removeRolloutPayloads(ctx context.Context, tx *sql.Tx, rollout string) error {
	if err := payloadCandidates(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO payload_candidates SELECT hash FROM payload_refs WHERE rollout_id=?`, rollout); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM payload_refs WHERE rollout_id=?`, rollout); err != nil {
		return err
	}
	return collectPayloads(ctx, tx)
}

func encodeValue(ctx context.Context, tx *sql.Tx, rollout, kind, key string, value any, shape *field12) ([]byte, int, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, 0, err
	}
	encoded, blobs, err := encode12(raw, shape)
	if err != nil {
		return nil, 0, err
	}
	if err = replacePayloadRefs(ctx, tx, rollout, kind, key, blobs); err != nil {
		return nil, 0, err
	}
	return encoded, len(raw), nil
}

type packedValue struct {
	raw   []byte
	shape *field12
}

// Hydrate only a selected page. One indexed query per bounded hash batch avoids
// a database round trip for every field. Callers close their initial rows first
// and keep this lookup in the same read snapshot.
func hydrate(ctx context.Context, tx *sql.Tx, values ...packedValue) ([][]byte, error) {
	packs := make([]packed12, len(values))
	hashes := map[string]bool{}
	for i, value := range values {
		if len(value.raw) == 0 {
			continue
		}
		if json.Unmarshal(value.raw, &packs[i]) != nil || len(packs[i].Body) == 0 {
			return nil, core.ErrCorrupt
		}
		for _, ref := range packs[i].Refs {
			hashes[ref.Hash] = true
		}
	}
	blobs := make(map[string][]byte, len(hashes))
	keys := make([]any, 0, len(hashes))
	for hash := range hashes {
		keys = append(keys, hash)
	}
	for start := 0; start < len(keys); start += 200 {
		end := min(start+200, len(keys))
		args := keys[start:end]
		rows, err := tx.QueryContext(ctx, `SELECT hash,byte_length,bytes FROM payloads WHERE hash IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var hash string
			var length int
			var raw []byte
			if err = rows.Scan(&hash, &length, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			if length != len(raw) || hash12(raw) != hash {
				rows.Close()
				return nil, fmt.Errorf("%w: invalid payload bytes", core.ErrCorrupt)
			}
			blobs[hash] = raw
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if len(blobs) != len(hashes) {
		return nil, fmt.Errorf("%w: missing payload", core.ErrCorrupt)
	}
	out := make([][]byte, len(values))
	for i, value := range values {
		if len(value.raw) == 0 {
			continue
		}
		raw, err := decode12(packs[i], value.shape, blobs)
		if err != nil {
			return nil, err
		}
		out[i] = raw
	}
	return out, nil
}

func decodeValue(ctx context.Context, tx *sql.Tx, raw []byte, shape *field12, value any) error {
	decoded, err := hydrate(ctx, tx, packedValue{raw, shape})
	if err != nil {
		return err
	}
	if err = json.Unmarshal(decoded[0], value); err != nil {
		return core.ErrCorrupt
	}
	return nil
}

func readState(ctx context.Context, tx *sql.Tx, raw []byte, id string) (core.RuntimeState, error) {
	decoded, err := hydrate(ctx, tx, packedValue{raw, state12})
	if err != nil {
		return core.RuntimeState{}, err
	}
	return decodeState(decoded[0], id)
}
