package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
)

const FormatVersion = 5
const ForkCommitID = "session-fork-created"
const DefaultPageSize = 256
const DefaultPageBytes = 4 << 20
const DefaultContextBytes = 32 << 20

// Store owns Rollout history, Session state, atomic commits and both Fork modes.
// Get/Scan take RolloutID; LoadState/Pending/Commit/LookupCommit take SessionID.
// Implementations return isolated values. They never execute models or tools.
// A successful Commit is durable for durable backends. Host-owned Close is
// deliberately absent: managers and sessions do not own the Store lifecycle.
type Store interface {
	LoadState(context.Context, string) (RuntimeState, error)
	Get(context.Context, string, string) (Record, error)
	Scan(context.Context, string, Query) (Page, error)
	Pending(context.Context, string, PendingQuery) (Page, error)
	Commit(context.Context, string, Commit) (Receipt, error)
	LookupCommit(context.Context, string, string) (Receipt, error)
	Fork(context.Context, ForkRequest) (RuntimeState, error)
}

// Record pairs immutable history with the branch projection at that node.
// Operational entries have no Projection.
type Record struct {
	Entry      Entry       `json:"entry"`
	Projection *Projection `json:"projection,omitempty"`
}

type Query struct {
	SessionID string // optional writer filter; historical provenance is preserved
	Cursor    string
	Limit     int
	MaxBytes  int
	// PathHead reads the parent chain backwards. StopID is included, then stops.
	PathHead    string
	StopID      string
	ParentID    *string
	Kind        EntryKind
	Reference   string
	Reverse     bool
	HistoryOnly bool
}
type PendingQuery struct {
	Kind      EntryKind
	InputKind InputKind
	Cursor    string
	Limit     int
	MaxBytes  int
}
type Page struct {
	Records []Record
	Next    string
}

// Commit IDs are stable across retries. The digest covers the expected version,
// entry identities and contents, excluding Store-assigned Sequence. Duplicate IDs with changed
// contents are conflicts, never another write.
type Commit struct {
	ID              string  `json:"id"`
	ExpectedVersion uint64  `json:"expected_version"`
	Entries         []Entry `json:"entries"`
}
type Receipt struct {
	FirstSequence  uint64 `json:"first_sequence"`
	LastSequence   uint64 `json:"last_sequence"`
	CommitID       string `json:"commit_id"`
	Version        uint64 `json:"version"`
	CurrentVersion uint64 `json:"current_version"`
	Digest         string `json:"digest"`
}

func (c Commit) Digest() (string, error) {
	normalized := c
	normalized.Entries = append([]Entry(nil), c.Entries...)
	for i := range normalized.Entries {
		normalized.Entries[i].Sequence = 0
	}
	b, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Confirmed restores the exact assigned entry order from a durable receipt.
func (c Commit) Confirmed(r Receipt) (Commit, error) {
	digest, err := c.Digest()
	if err != nil {
		return Commit{}, err
	}
	if r.CommitID != c.ID || r.Digest != digest || r.Version != c.ExpectedVersion+1 || len(c.Entries) == 0 || r.FirstSequence == 0 || r.LastSequence < r.FirstSequence || r.LastSequence-r.FirstSequence != uint64(len(c.Entries)-1) {
		return Commit{}, fmt.Errorf("%w: commit receipt mismatch", ErrCorrupt)
	}
	out, err := copyJSON(c)
	if err != nil {
		return Commit{}, err
	}
	for i := range out.Entries {
		out.Entries[i].Sequence = r.FirstSequence + uint64(i)
	}
	return out, nil
}

// AssignCommit is called by a Store under its rollout write transaction.
func AssignCommit(c Commit, first uint64) (Commit, Receipt, error) {
	if len(c.Entries) == 0 || first == 0 || first > math.MaxInt64 || uint64(len(c.Entries)-1) > math.MaxInt64-first {
		return Commit{}, Receipt{}, ErrCorrupt
	}
	for _, e := range c.Entries {
		if e.Sequence != 0 {
			return Commit{}, Receipt{}, fmt.Errorf("%w: sequence is Store-assigned", ErrCorrupt)
		}
	}
	digest, err := c.Digest()
	if err != nil {
		return Commit{}, Receipt{}, err
	}
	r := Receipt{CommitID: c.ID, Digest: digest, Version: c.ExpectedVersion + 1, CurrentVersion: c.ExpectedVersion + 1, FirstSequence: first, LastSequence: first + uint64(len(c.Entries)) - 1}
	confirmed, err := c.Confirmed(r)
	return confirmed, r, err
}

type ForkRequest struct {
	SourceID        string
	SourceRolloutID string
	TargetID        string
	HeadID          string
	Mode            ForkMode
}

func (r ForkRequest) Digest() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// RolloutID is deterministic for a detached target's stable creation identity.
func (r ForkRequest) RolloutID() string {
	if r.Mode == ForkShared {
		return r.SourceRolloutID
	}
	sum := sha256.Sum256([]byte(r.TargetID))
	return "fork-" + hex.EncodeToString(sum[:])
}

func PageLimits(limit, maxBytes int) (int, int) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if maxBytes <= 0 {
		maxBytes = DefaultPageBytes
	}
	return limit, maxBytes
}
func Reference(e Entry) string {
	if e.InputID != "" {
		return e.InputID
	}
	if e.Tool != nil {
		return e.Tool.Invocation.ExecutionID
	}
	if e.Approval != nil {
		return e.Approval.ID
	}
	if e.Input != nil {
		return e.Input.ID
	}
	return ""
}
