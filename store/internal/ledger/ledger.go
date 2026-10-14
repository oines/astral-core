// Package ledger is the in-process index used by the memory and JSONL adapters.
// SQLite keeps these indexes in the database instead.
package ledger

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"

	core "github.com/oines/astral-core"
	s "github.com/oines/astral-core/session"
)

type Data struct {
	State      s.RuntimeState
	Records    map[string]s.Record
	Order      []string
	History    []string
	Children   map[string][]string
	References map[string][]string
	Pending    map[string]s.Record
	Receipts   map[string]s.Receipt
	Ready      bool
}
type Store struct {
	Mu       sync.RWMutex
	Sessions map[string]*Data
}

func New() *Store { return &Store{Sessions: map[string]*Data{}} }
func NewData() *Data {
	return &Data{Records: map[string]s.Record{}, Children: map[string][]string{}, References: map[string][]string{}, Pending: map[string]s.Record{}, Receipts: map[string]s.Receipt{}}
}
func Clone[T any](v T) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}
func (d *Data) Add(r s.Record) {
	e := r.Entry
	d.Records[e.ID] = r
	d.Order = append(d.Order, e.ID)
	if e.IsHistory() {
		d.History = append(d.History, e.ID)
		d.Children[e.ParentID] = append(d.Children[e.ParentID], e.ID)
	}
	for _, key := range []string{string(e.Kind) + "\x00", string(e.Kind) + "\x00" + s.Reference(e)} {
		if key == string(e.Kind)+"\x00" && s.Reference(e) == "" {
			continue
		}
		d.References[key] = append(d.References[key], e.ID)
	}
	if s.Reference(e) == "" {
		key := string(e.Kind) + "\x00"
		d.References[key] = append(d.References[key], e.ID)
	}
	switch e.Kind {
	case s.EntryInputQueued:
		d.Pending["input:"+e.Input.ID] = r
	case s.EntryInputConsumed:
		delete(d.Pending, "input:"+e.Input.ID)
	case s.EntryApprovalRequested:
		d.Pending["approval:"+e.Approval.ID] = r
	case s.EntryApprovalDecided:
		delete(d.Pending, "approval:"+e.Approval.ID)
	case s.EntryToolResult, s.EntryToolResolution:
		for key, p := range d.Pending {
			if p.Entry.Approval != nil && p.Entry.Approval.ExecutionID == e.Tool.Invocation.ExecutionID {
				delete(d.Pending, key)
			}
		}
	case s.EntryMessage, s.EntryHeadSelected:
		for key, p := range d.Pending {
			if p.Entry.Approval != nil {
				delete(d.Pending, key)
			}
		}
	}
}
func (d *Data) Sort() {
	less := func(ids []string) {
		sort.Slice(ids, func(i, j int) bool { return d.Records[ids[i]].Entry.Sequence < d.Records[ids[j]].Entry.Sequence })
	}
	less(d.Order)
	less(d.History)
	for _, ids := range d.Children {
		less(ids)
	}
	for _, ids := range d.References {
		less(ids)
	}
}
func (st *Store) LoadState(ctx context.Context, id string) (s.RuntimeState, error) {
	if err := ctx.Err(); err != nil {
		return s.RuntimeState{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil || !d.Ready {
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
	d := st.Sessions[id]
	if d == nil || !d.Ready {
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
	if d == nil || !d.Ready {
		return s.Receipt{}, s.ErrNotFound
	}
	r, ok := d.Receipts[key]
	if !ok {
		return s.Receipt{}, s.ErrNotFound
	}
	r.CurrentVersion = d.State.Version
	return r, nil
}

// Prepare does not mutate Data. The JSONL adapter writes and syncs this complete
// commit before calling Apply, so an IO error cannot advance its in-memory view.
func (d *Data) Prepare(id string, c s.Commit) (s.RuntimeState, []s.Record, s.Receipt, error) {
	digest, err := c.Digest()
	if err != nil {
		return s.RuntimeState{}, nil, s.Receipt{}, err
	}
	if old, ok := d.Receipts[c.ID]; ok {
		if old.Digest != digest {
			return s.RuntimeState{}, nil, s.Receipt{}, s.ErrConflict
		}
		old.CurrentVersion = d.State.Version
		return d.State, nil, old, nil
	}
	if d.State.Version > 0 && !d.Ready {
		return s.RuntimeState{}, nil, s.Receipt{}, s.ErrConflict
	}
	if c.ExpectedVersion != d.State.Version {
		return s.RuntimeState{}, nil, s.Receipt{}, s.ErrConflict
	}
	state, records, err := s.Reduce(d.State, c)
	if err != nil {
		return s.RuntimeState{}, nil, s.Receipt{}, err
	}
	if state.SessionID != id {
		return s.RuntimeState{}, nil, s.Receipt{}, s.ErrCorrupt
	}
	queued := map[string]bool{}
	for _, e := range c.Entries {
		if _, ok := d.Records[e.ID]; ok {
			return s.RuntimeState{}, nil, s.Receipt{}, s.ErrCorrupt
		}
		if e.Head != nil {
			r, ok := d.Records[e.Head.EntryID]
			if !ok || r.Projection == nil || !Equal(r.Projection, c.Selection) {
				return s.RuntimeState{}, nil, s.Receipt{}, s.ErrCorrupt
			}
		}
		if e.Compaction != nil {
			r, ok := d.Records[e.Compaction.ThroughID]
			if !ok || r.Projection == nil {
				return s.RuntimeState{}, nil, s.Receipt{}, s.ErrCorrupt
			}
		}
		if e.Kind == s.EntryInputConsumed {
			p, ok := d.Pending["input:"+e.Input.ID]
			if !ok || p.Entry.Input.Kind != e.Input.Kind {
				return s.RuntimeState{}, nil, s.Receipt{}, s.ErrCorrupt
			}
		}
		if e.Kind == s.EntryInputQueued {
			_, exists := d.Pending["input:"+e.Input.ID]
			if exists || queued[e.Input.ID] {
				return s.RuntimeState{}, nil, s.Receipt{}, s.ErrCorrupt
			}
			queued[e.Input.ID] = true
		}
	}
	receipt := s.Receipt{CommitID: c.ID, Version: state.Version, CurrentVersion: state.Version, Digest: digest}
	return state, records, receipt, nil
}
func Equal(a, b any) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && string(x) == string(y)
}
func (d *Data) Apply(state s.RuntimeState, records []s.Record, r s.Receipt) {
	for _, record := range records {
		d.Add(record)
	}
	d.State = state
	d.Ready = true
	d.Receipts[r.CommitID] = r
}
func (st *Store) Commit(ctx context.Context, id string, c s.Commit) (s.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return s.Receipt{}, err
	}
	if id == "" {
		return s.Receipt{}, core.ErrInvalid
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.Receipt{}, err
	}
	d := st.Sessions[id]
	if d == nil {
		d = NewData()
	} else if !d.Ready {
		return s.Receipt{}, s.ErrConflict
	}
	state, records, r, err := d.Prepare(id, c)
	if err != nil {
		return s.Receipt{}, err
	}
	if len(records) > 0 {
		d.Apply(state, records, r)
		st.Sessions[id] = d
	}
	return r, nil
}
func (st *Store) Scan(ctx context.Context, id string, q s.Query) (s.Page, error) {
	if err := ctx.Err(); err != nil {
		return s.Page{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil || !d.Ready {
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
			if q.Kind != "" && r.Entry.Kind != q.Kind || q.Reference != "" && s.Reference(r.Entry) != q.Reference || q.HistoryOnly && !r.Entry.IsHistory() {
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
	if d == nil || !d.Ready {
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

// ForkInto copies in bounded reverse pages into a private destination index.
// The caller controls durable staging and publication. Source records are immutable.
func (st *Store) ForkInto(ctx context.Context, request s.ForkRequest, emit func([]s.Record) error) (s.RuntimeState, error) {
	head, err := st.Get(ctx, request.SourceID, request.HeadID)
	if err != nil {
		return s.RuntimeState{}, err
	}
	state, err := s.ForkState(request, head)
	if err != nil {
		return s.RuntimeState{}, err
	}
	creation := s.Record{Entry: request.Creation}
	if err = emit([]s.Record{creation}); err != nil {
		return s.RuntimeState{}, err
	}
	cursor := ""
	for {
		p, err := st.Scan(ctx, request.SourceID, s.Query{PathHead: request.HeadID, Cursor: cursor, Limit: s.DefaultPageSize, MaxBytes: s.DefaultContextBytes})
		if err != nil {
			return s.RuntimeState{}, err
		}
		for i := range p.Records {
			p.Records[i].Entry.Version = 1
		}
		if err = emit(p.Records); err != nil {
			return s.RuntimeState{}, err
		}
		if p.Next == "" {
			break
		}
		cursor = p.Next
	}
	if request.Environment != nil {
		c := s.Commit{ID: request.Environment.ID, ExpectedVersion: state.Version, Entries: []s.Entry{*request.Environment}}
		next, records, err := s.Reduce(state, c)
		if err != nil {
			return s.RuntimeState{}, err
		}
		state = next
		if err = emit(records); err != nil {
			return s.RuntimeState{}, err
		}
	}
	return state, nil
}
func (st *Store) Fork(ctx context.Context, request s.ForkRequest) (s.RuntimeState, error) {
	if err := s.ValidateFork(request); err != nil {
		return s.RuntimeState{}, err
	}
	st.Mu.Lock()
	if st.Sessions[request.TargetID] != nil {
		st.Mu.Unlock()
		return s.RuntimeState{}, s.ErrConflict
	}
	d := NewData()
	st.Sessions[request.TargetID] = d
	st.Mu.Unlock()
	state, err := st.ForkInto(ctx, request, func(records []s.Record) error {
		owned, err := Clone(records)
		if err != nil {
			return err
		}
		for _, r := range owned {
			d.Add(r)
		}
		return ctx.Err()
	})
	st.Mu.Lock()
	defer st.Mu.Unlock()
	if err != nil {
		delete(st.Sessions, request.TargetID)
		return s.RuntimeState{}, err
	}
	d.State = state
	d.Sort()
	d.Ready = true
	return Clone(state)
}

var _ s.Store = (*Store)(nil)
