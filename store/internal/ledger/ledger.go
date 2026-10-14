// Package ledger indexes Rollout history separately from execution state.
package ledger

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"

	core "github.com/oines/astral-core"
	s "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/internal/historypage"
	"github.com/oines/astral-core/store/internal/targetlock"
)

type Data struct {
	State    s.RuntimeState
	Pending  map[string]s.Record
	Receipts map[string]s.Receipt
}
type Rollout struct {
	ID                   string
	Status               string
	Sequence             uint64
	Records              map[string]s.Record
	Order, History       []string
	Children, References map[string][]string
}
type Store struct {
	Mu       sync.RWMutex
	Sessions map[string]*Data
	Rollouts map[string]*Rollout
	forks    targetlock.Map
}
type Change struct {
	Commit  s.Commit
	State   s.RuntimeState
	Records []s.Record
	Receipt s.Receipt
}

func New() *Store    { return &Store{Sessions: map[string]*Data{}, Rollouts: map[string]*Rollout{}} }
func NewData() *Data { return &Data{Pending: map[string]s.Record{}, Receipts: map[string]s.Receipt{}} }
func NewRollout(id string) *Rollout {
	return &Rollout{ID: id, Status: "active", Records: map[string]s.Record{}, Children: map[string][]string{}, References: map[string][]string{}}
}
func Clone[T any](v T) (T, error) {
	var out T
	b, e := json.Marshal(v)
	if e != nil {
		return out, e
	}
	e = json.Unmarshal(b, &out)
	return out, e
}
func Equal(a, b any) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && string(x) == string(y)
}
func (r *Rollout) Add(record s.Record) {
	e := record.Entry
	r.Records[e.ID] = record
	r.Order = append(r.Order, e.ID)
	if e.Sequence > r.Sequence {
		r.Sequence = e.Sequence
	}
	if e.IsHistory() {
		r.History = append(r.History, e.ID)
		r.Children[e.ParentID] = append(r.Children[e.ParentID], e.ID)
	}
	key := string(e.Kind) + "\x00"
	r.References[key] = append(r.References[key], e.ID)
	if ref := s.Reference(e); ref != "" {
		r.References[key+ref] = append(r.References[key+ref], e.ID)
	}
}
func (r *Rollout) Sort() {
	less := func(ids []string) {
		sort.Slice(ids, func(i, j int) bool { return r.Records[ids[i]].Entry.Sequence < r.Records[ids[j]].Entry.Sequence })
	}
	less(r.Order)
	less(r.History)
	for _, v := range r.Children {
		less(v)
	}
	for _, v := range r.References {
		less(v)
	}
}
func (d *Data) AddPending(r s.Record) {
	e := r.Entry
	switch e.Kind {
	case s.EntryInputQueued:
		d.Pending["input:"+e.Input.ID] = r
	case s.EntryInputConsumed:
		delete(d.Pending, "input:"+e.Input.ID)
	case s.EntryApprovalRequested:
		d.Pending["approval:"+e.Approval.ID] = r
	case s.EntryApprovalDecided:
		delete(d.Pending, "approval:"+e.Approval.ID)
	case s.EntryToolResult, s.EntryToolResolution, s.EntryMessage:
		for key, p := range d.Pending {
			if p.Entry.Approval != nil && (e.Kind == s.EntryMessage || p.Entry.Approval.ExecutionID == e.Tool.Invocation.ExecutionID) {
				delete(d.Pending, key)
			}
		}
	}
}
func (st *Store) LoadState(ctx context.Context, id string) (s.RuntimeState, error) {
	if err := ctx.Err(); err != nil {
		return s.RuntimeState{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil || st.Rollouts[d.State.RolloutID] == nil || st.Rollouts[d.State.RolloutID].Status != "active" {
		return s.RuntimeState{}, s.ErrNotFound
	}
	return Clone(d.State)
}
func (st *Store) Get(ctx context.Context, id, key string) (s.Record, error) {
	if err := ctx.Err(); err != nil {
		return s.Record{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Rollouts[id]
	if d == nil || d.Status != "active" {
		return s.Record{}, s.ErrNotFound
	}
	r, ok := d.Records[key]
	if !ok {
		return s.Record{}, s.ErrNotFound
	}
	return Clone(r)
}
func (st *Store) LookupCommit(ctx context.Context, id, key string) (s.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return s.Receipt{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil || st.Rollouts[d.State.RolloutID] == nil || st.Rollouts[d.State.RolloutID].Status != "active" {
		return s.Receipt{}, s.ErrNotFound
	}
	r, ok := d.Receipts[key]
	if !ok {
		return s.Receipt{}, s.ErrNotFound
	}
	r.CurrentVersion = d.State.Version
	return r, nil
}

// Prepare is non-mutating and requires Mu. Durable adapters sync before Apply.
func (st *Store) Prepare(id string, c s.Commit) (Change, error) {
	if id == "" || len(c.Entries) == 0 || c.ID == s.ForkCommitID {
		return Change{}, core.ErrInvalid
	}
	var rolloutID string
	d := st.Sessions[id]
	if d != nil {
		rolloutID = d.State.RolloutID
	} else if c.Entries[0].Created != nil && c.Entries[0].Created.Projection == nil {
		rolloutID = c.Entries[0].Created.RolloutID
	} else {
		return Change{}, s.ErrCorrupt
	}
	r := st.Rollouts[rolloutID]
	if r == nil {
		if d != nil || rolloutID == "" {
			return Change{}, s.ErrCorrupt
		}
		r = NewRollout(rolloutID)
	} else if r.Status != "active" {
		return Change{}, s.ErrNotFound
	} else if d == nil {
		return Change{}, s.ErrConflict
	}
	digest, err := c.Digest()
	if err != nil {
		return Change{}, err
	}
	if d != nil {
		if old, ok := d.Receipts[c.ID]; ok {
			if old.Digest != digest {
				return Change{}, s.ErrConflict
			}
			old.CurrentVersion = d.State.Version
			return Change{Receipt: old, State: d.State}, nil
		}
	}
	confirmed, receipt, err := s.AssignCommit(c, r.Sequence+1)
	if err != nil {
		return Change{}, err
	}
	return st.PrepareConfirmed(id, confirmed, receipt)
}

// PrepareConfirmed validates an exact durable commit, including Fork seeds.
func (st *Store) PrepareConfirmed(id string, c s.Commit, receipt s.Receipt) (Change, error) {
	d := st.Sessions[id]
	if d == nil {
		d = NewData()
	}
	if c.ExpectedVersion != d.State.Version {
		return Change{}, s.ErrConflict
	}
	if len(c.Entries) == 0 || receipt.FirstSequence == 0 || receipt.LastSequence-receipt.FirstSequence != uint64(len(c.Entries)-1) || receipt.Version != c.ExpectedVersion+1 || receipt.CommitID != c.ID {
		return Change{}, s.ErrCorrupt
	}
	for i, e := range c.Entries {
		if e.Sequence != receipt.FirstSequence+uint64(i) {
			return Change{}, s.ErrCorrupt
		}
	}
	digest, err := c.Digest()
	if err != nil {
		return Change{}, err
	}
	if c.ID == s.ForkCommitID {
		if c.Entries[0].Created == nil {
			return Change{}, s.ErrCorrupt
		}
		digest = c.Entries[0].Created.ForkDigest
	}
	if digest != receipt.Digest {
		return Change{}, s.ErrCorrupt
	}
	state, records, err := s.Reduce(d.State, c)
	if err != nil {
		return Change{}, err
	}
	if state.SessionID != id {
		return Change{}, s.ErrCorrupt
	}
	r := st.Rollouts[state.RolloutID]
	if r == nil {
		r = NewRollout(state.RolloutID)
	}
	if (r.Status != "active" && !(r.Status == "building" && c.ID == s.ForkCommitID && d.State.Version == 0)) || receipt.FirstSequence != r.Sequence+1 {
		return Change{}, s.ErrCorrupt
	}
	if c.Entries[0].Created != nil && c.Entries[0].Created.Projection != nil {
		head, ok := r.Records[c.Entries[0].Created.ForkEntryID]
		if !ok || !Equal(head.Projection, c.Entries[0].Created.Projection) {
			return Change{}, s.ErrCorrupt
		}
	}
	queued := map[string]bool{}
	for _, e := range c.Entries {
		if _, ok := r.Records[e.ID]; ok {
			return Change{}, s.ErrCorrupt
		}
		if e.Compaction != nil {
			rec, ok := r.Records[e.Compaction.ThroughID]
			if !ok || rec.Projection == nil {
				return Change{}, s.ErrCorrupt
			}
		}
		if e.Kind == s.EntryInputConsumed {
			p, ok := d.Pending["input:"+e.Input.ID]
			if !ok || p.Entry.Input.Kind != e.Input.Kind {
				return Change{}, s.ErrCorrupt
			}
		}
		if e.Kind == s.EntryInputQueued {
			_, ok := d.Pending["input:"+e.Input.ID]
			if ok || queued[e.Input.ID] {
				return Change{}, s.ErrCorrupt
			}
			queued[e.Input.ID] = true
		}
	}
	return Change{Commit: c, State: state, Records: records, Receipt: receipt}, nil
}
func (st *Store) Apply(change Change) {
	id := change.State.SessionID
	d := st.Sessions[id]
	if d == nil {
		d = NewData()
		st.Sessions[id] = d
	}
	r := st.Rollouts[change.State.RolloutID]
	if r == nil {
		r = NewRollout(change.State.RolloutID)
		st.Rollouts[r.ID] = r
	}
	r.Status = "active"
	for _, record := range change.Records {
		r.Add(record)
		d.AddPending(record)
	}
	d.State = change.State
	d.Receipts[change.Receipt.CommitID] = change.Receipt
}
func (st *Store) Commit(ctx context.Context, id string, c s.Commit) (s.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return s.Receipt{}, err
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	change, err := st.Prepare(id, c)
	if err != nil {
		return s.Receipt{}, err
	}
	if err = ctx.Err(); err != nil {
		return s.Receipt{}, err
	}
	if len(change.Records) > 0 {
		st.Apply(change)
	}
	return change.Receipt, nil
}
func (st *Store) Scan(ctx context.Context, id string, q s.Query) (s.Page, error) {
	if err := ctx.Err(); err != nil {
		return s.Page{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Rollouts[id]
	if d == nil || d.Status != "active" {
		return s.Page{}, s.ErrNotFound
	}
	limit, maxBytes := s.PageLimits(q.Limit, q.MaxBytes)
	page := s.Page{}
	size := 0
	add := func(r s.Record) (bool, error) {
		b, err := json.Marshal(r)
		if err != nil {
			return false, err
		}
		if size+len(b) > maxBytes {
			if len(page.Records) == 0 {
				return false, &s.BudgetError{Input: len(b), Window: maxBytes}
			}
			return false, nil
		}
		page.Records = append(page.Records, r)
		size += len(b)
		return true, nil
	}
	if q.PathHead != "" {
		key := q.PathHead
		if q.Cursor != "" {
			key = q.Cursor
		}
		first := true
		for key != "" {
			if err := ctx.Err(); err != nil {
				return s.Page{}, err
			}
			r, ok := d.Records[key]
			if !ok || !r.Entry.IsHistory() {
				if first {
					return s.Page{}, s.ErrNotFound
				}
				return s.Page{}, s.ErrCorrupt
			}
			first = false
			ok, err := add(r)
			if err != nil {
				return s.Page{}, err
			}
			if !ok {
				page.Next = key
				break
			}
			if key == q.StopID {
				break
			}
			key = r.Entry.ParentID
			if len(page.Records) == limit {
				page.Next = key
				break
			}
		}
	} else {
		ids := d.Order
		if q.ParentID != nil {
			ids = d.Children[*q.ParentID]
		} else if q.Kind != "" {
			ids = d.References[string(q.Kind)+"\x00"+q.Reference]
		} else if q.HistoryOnly {
			ids = d.History
		}
		cursor := uint64(0)
		if q.Cursor != "" {
			var err error
			cursor, err = strconv.ParseUint(q.Cursor, 10, 64)
			if err != nil {
				return s.Page{}, core.ErrInvalid
			}
		}
		start, step := 0, 1
		if q.Reverse {
			start = len(ids) - 1
			step = -1
		}
		if q.Cursor != "" {
			if q.Reverse {
				start = sort.Search(len(ids), func(i int) bool { return d.Records[ids[i]].Entry.Sequence >= cursor }) - 1
			} else {
				start = sort.Search(len(ids), func(i int) bool { return d.Records[ids[i]].Entry.Sequence > cursor })
			}
		}
		for i := start; i >= 0 && i < len(ids); i += step {
			if err := ctx.Err(); err != nil {
				return s.Page{}, err
			}
			r := d.Records[ids[i]]
			if q.SessionID != "" && r.Entry.SessionID != q.SessionID || q.Kind != "" && r.Entry.Kind != q.Kind || q.Reference != "" && s.Reference(r.Entry) != q.Reference || q.HistoryOnly && !r.Entry.IsHistory() {
				continue
			}
			if len(page.Records) == limit {
				page.Next = strconv.FormatUint(page.Records[len(page.Records)-1].Entry.Sequence, 10)
				break
			}
			ok, err := add(r)
			if err != nil {
				return s.Page{}, err
			}
			if !ok {
				page.Next = strconv.FormatUint(page.Records[len(page.Records)-1].Entry.Sequence, 10)
				break
			}
		}
	}
	return Clone(page)
}
func (st *Store) Pending(ctx context.Context, id string, q s.PendingQuery) (s.Page, error) {
	if err := ctx.Err(); err != nil {
		return s.Page{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil || st.Rollouts[d.State.RolloutID] == nil || st.Rollouts[d.State.RolloutID].Status != "active" {
		return s.Page{}, s.ErrNotFound
	}
	var rows []s.Record
	for _, r := range d.Pending {
		if q.Kind != "" && r.Entry.Kind != q.Kind {
			continue
		}
		if q.InputKind != "" && (r.Entry.Input == nil || r.Entry.Input.Kind != q.InputKind) {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Entry.Sequence < rows[j].Entry.Sequence })
	limit, maxBytes := s.PageLimits(q.Limit, q.MaxBytes)
	cursor := uint64(0)
	if q.Cursor != "" {
		var err error
		cursor, err = strconv.ParseUint(q.Cursor, 10, 64)
		if err != nil {
			return s.Page{}, core.ErrInvalid
		}
	}
	page := s.Page{}
	size := 0
	for _, r := range rows {
		if r.Entry.Sequence <= cursor {
			continue
		}
		b, _ := json.Marshal(r)
		if len(page.Records) == limit || size+len(b) > maxBytes {
			if len(page.Records) == 0 {
				return s.Page{}, s.ErrBudget
			}
			page.Next = strconv.FormatUint(page.Records[len(page.Records)-1].Entry.Sequence, 10)
			break
		}
		page.Records = append(page.Records, r)
		size += len(b)
	}
	return Clone(page)
}

// LockFork serializes only the same target; unrelated parent writes can proceed.
func (st *Store) LockFork(ctx context.Context, id string) (func(), error) {
	return st.forks.Lock(ctx, id)
}
func (st *Store) ExistingFork(r s.ForkRequest) (s.RuntimeState, bool, error) {
	d := st.Sessions[r.TargetID]
	if d == nil {
		return s.RuntimeState{}, false, nil
	}
	if rollout := st.Rollouts[d.State.RolloutID]; rollout == nil || rollout.Status != "active" {
		return s.RuntimeState{}, true, s.ErrNotFound
	}
	digest, err := r.Digest()
	if err != nil {
		return s.RuntimeState{}, true, err
	}
	receipt, ok := d.Receipts[s.ForkCommitID]
	if !ok || receipt.Digest != digest {
		return s.RuntimeState{}, true, s.ErrConflict
	}
	state, err := Clone(d.State)
	return state, true, err
}
func (st *Store) Fork(ctx context.Context, r s.ForkRequest) (s.RuntimeState, error) {
	if err := s.ValidateFork(r); err != nil {
		return s.RuntimeState{}, err
	}
	unlock, err := st.LockFork(ctx, r.TargetID)
	if err != nil {
		return s.RuntimeState{}, err
	}
	defer unlock()
	st.Mu.RLock()
	state, found, err := st.ExistingFork(r)
	source := st.Sessions[r.SourceID]
	validSource := source != nil && source.State.RolloutID == r.SourceRolloutID
	st.Mu.RUnlock()
	if found || err != nil {
		return state, err
	}
	if !validSource {
		return state, s.ErrNotFound
	}
	head, err := st.Get(ctx, r.SourceRolloutID, r.HeadID)
	if err != nil {
		return state, err
	}
	if _, err = s.ForkState(r, head); err != nil {
		return state, err
	}
	var detached *Rollout
	if r.Mode == s.ForkDetached {
		detached = NewRollout(r.RolloutID())
		cursor := ""
		for {
			page, e := historypage.Read(ctx, st, r.SourceRolloutID, r.HeadID, cursor)
			if e != nil {
				return state, e
			}
			for _, record := range page.Records {
				detached.Add(record)
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
		detached.Sort()
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if err = ctx.Err(); err != nil {
		return state, err
	}
	if state, found, err = st.ExistingFork(r); found || err != nil {
		return state, err
	}
	rollout := st.Rollouts[r.SourceRolloutID]
	if rollout == nil || rollout.Status != "active" {
		return state, s.ErrNotFound
	}
	if detached != nil {
		if st.Rollouts[detached.ID] != nil {
			return state, s.ErrConflict
		}
		rollout = detached
	}
	c, state, records, receipt, err := s.PrepareFork(r, head, rollout.Sequence+1)
	if err != nil {
		return state, err
	}
	if detached != nil {
		st.Rollouts[detached.ID] = detached
	}
	st.Apply(Change{Commit: c, State: state, Records: records, Receipt: receipt})
	return Clone(state)
}

var _ s.Store = (*Store)(nil)
