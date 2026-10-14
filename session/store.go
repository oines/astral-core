package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const FormatVersion = 2
const DefaultPageSize = 256
const DefaultPageBytes = 4 << 20
const DefaultContextBytes = 32 << 20

// Store owns atomic commits, indexed reads and independent streaming forks.
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
	Cursor   string
	Limit    int
	MaxBytes int
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
// assigned entry identities and selected projection. Duplicate IDs with changed
// contents are conflicts, never another write.
type Commit struct {
	ID              string      `json:"id"`
	ExpectedVersion uint64      `json:"expected_version"`
	Entries         []Entry     `json:"entries"`
	Selection       *Projection `json:"selection,omitempty"`
}
type Receipt struct {
	CommitID       string `json:"commit_id"`
	Version        uint64 `json:"version"`
	CurrentVersion uint64 `json:"current_version"`
	Digest         string `json:"digest"`
}

func (c Commit) Digest() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type ForkRequest struct {
	SourceID        string
	TargetID        string
	HeadID          string
	SourceVersion   uint64
	AllowIncomplete bool
	Creation        Entry
	// Optional fresh environment; absent means rediscovery before new effects.
	Environment *Entry
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
