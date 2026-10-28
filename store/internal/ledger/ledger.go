// Package ledger indexes Rollout history separately from execution state.
package ledger

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"

	core "github.com/oines/astral-core"

	"github.com/oines/astral-core/store/internal/historypage"
	"github.com/oines/astral-core/store/internal/pending"
	"github.com/oines/astral-core/store/internal/targetlock"
)

type Data struct {
	State        core.RuntimeState
	Pending      map[string]core.Record
	Receipts     map[string]core.Receipt
	Inputs       map[string]core.InputReceipt
	Attempts     map[string]core.ModelAttempt
	AttemptOrder []string
}
type Rollout struct {
	ID                   string
	Status               string
	Sequence             uint64
	Records              map[string]core.Record
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
	Commit   core.Commit
	State    core.RuntimeState
	Records  []core.Record
	Receipt  core.Receipt
	Attempts []core.ModelAttempt
}

func New() *Store { return &Store{Sessions: map[string]*Data{}, Rollouts: map[string]*Rollout{}} }
func NewData() *Data {
	return &Data{Pending: map[string]core.Record{}, Receipts: map[string]core.Receipt{}, Inputs: map[string]core.InputReceipt{}, Attempts: map[string]core.ModelAttempt{}}
}
func NewRollout(id string) *Rollout {
	return &Rollout{ID: id, Status: "active", Records: map[string]core.Record{}, Children: map[string][]string{}, References: map[string][]string{}}
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
func (r *Rollout) Add(record core.Record) {
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
	if ref := core.Reference(e); ref != "" {
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
func (d *Data) AddPending(r core.Record) {
	e := r.Entry
	if e.InputID != "" && (e.Kind == core.EntryMessage || e.Kind == core.EntryContext) {
		receipt := d.Inputs[e.InputID]
		if receipt.EntryID == "" || e.Kind == core.EntryMessage {
			receipt.EntryID = e.ID
		}
		d.Inputs[e.InputID] = receipt
	}
	switch e.Kind {
	case core.EntryInputRequeued:
		receipt := d.Inputs[e.Input.ID]
		receipt.InputRecord, receipt.State = *e.Input, core.InputPending
		d.Inputs[e.Input.ID] = receipt
		d.Pending["input:"+e.Input.ID] = r
	case core.EntryInputQueued:
		d.Pending["input:"+e.Input.ID] = r
		d.Inputs[e.Input.ID] = core.InputReceipt{InputRecord: *e.Input, AcceptedAt: e.Time, State: core.InputPending}
	case core.EntryInputCleared:
		if e.Input != nil {
			receipt := d.Inputs[e.Input.ID]
			receipt.State = core.InputCleared
			d.Inputs[e.Input.ID] = receipt
			delete(d.Pending, "input:"+e.Input.ID)
			break
		}
		for key, p := range d.Pending {
			if p.Entry.Kind == core.EntryInputQueued || p.Entry.Kind == core.EntryInputRequeued {
				for _, kind := range e.Clear.Kinds {
					if p.Entry.Input.Kind == kind {
						receipt := d.Inputs[p.Entry.Input.ID]
						receipt.State = core.InputCleared
						d.Inputs[p.Entry.Input.ID] = receipt
						delete(d.Pending, key)
						break
					}
				}
			}
		}
	case core.EntryInputConsumed:
		delete(d.Pending, "input:"+e.Input.ID)
		receipt := d.Inputs[e.Input.ID]
		receipt.State = core.InputApplied
		d.Inputs[e.Input.ID] = receipt
	case core.EntryApprovalRequested:
		d.Pending["approval:"+e.Approval.ID] = r
	case core.EntryApprovalDecided:
		delete(d.Pending, "approval:"+e.Approval.ID)
	case core.EntryToolResult, core.EntryToolResolution, core.EntryToolControl, core.EntryToolUnresolved, core.EntryMessage:
		for key, p := range d.Pending {
			if p.Entry.Approval != nil && (e.Kind == core.EntryMessage || p.Entry.Approval.ExecutionID == e.Tool.Invocation.ExecutionID) {
				delete(d.Pending, key)
			}
		}
	}
}
func (st *Store) LoadState(ctx context.Context, id string) (core.RuntimeState, error) {
	if err := ctx.Err(); err != nil {
		return core.RuntimeState{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil || st.Rollouts[d.State.RolloutID] == nil || st.Rollouts[d.State.RolloutID].Status != "active" {
		return core.RuntimeState{}, core.ErrNotFound
	}
	return Clone(d.State)
}
func (st *Store) Get(ctx context.Context, id, key string) (core.Record, error) {
	if err := ctx.Err(); err != nil {
		return core.Record{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Rollouts[id]
	if d == nil || d.Status != "active" {
		return core.Record{}, core.ErrNotFound
	}
	r, ok := d.Records[key]
	if !ok {
		return core.Record{}, core.ErrNotFound
	}
	return Clone(r)
}
func (st *Store) LookupCommit(ctx context.Context, id, key string) (core.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return core.Receipt{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil || st.Rollouts[d.State.RolloutID] == nil || st.Rollouts[d.State.RolloutID].Status != "active" {
		return core.Receipt{}, core.ErrNotFound
	}
	r, ok := d.Receipts[key]
	if !ok {
		return core.Receipt{}, core.ErrNotFound
	}
	r.CurrentVersion = d.State.Version
	return r, nil
}

// Prepare is non-mutating and requires Mu. Durable adapters sync before Apply.
func (st *Store) Prepare(id string, c core.Commit) (Change, error) {
	if id == "" || len(c.Entries)+len(c.Attempts) == 0 || c.ID == core.ForkCommitID {
		return Change{}, core.ErrInvalid
	}
	var rolloutID string
	d := st.Sessions[id]
	if d != nil {
		rolloutID = d.State.RolloutID
	} else if len(c.Entries) > 0 && c.Entries[0].Created != nil && c.Entries[0].Created.Projection == nil {
		rolloutID = c.Entries[0].Created.RolloutID
	} else {
		return Change{}, core.ErrCorrupt
	}
	r := st.Rollouts[rolloutID]
	if r == nil {
		if d != nil || rolloutID == "" {
			return Change{}, core.ErrCorrupt
		}
		r = NewRollout(rolloutID)
	} else if r.Status != "active" {
		return Change{}, core.ErrNotFound
	} else if d == nil {
		return Change{}, core.ErrConflict
	}
	digest, err := c.Digest()
	if err != nil {
		return Change{}, err
	}
	if d != nil {
		if old, ok := d.Receipts[c.ID]; ok {
			if old.Digest != digest {
				return Change{}, core.ErrConflict
			}
			old.CurrentVersion = d.State.Version
			return Change{Receipt: old, State: d.State}, nil
		}
	}
	confirmed, receipt, err := core.AssignCommit(c, r.Sequence+1)
	if err != nil {
		return Change{}, err
	}
	return st.PrepareConfirmed(id, confirmed, receipt)
}

// PrepareConfirmed validates an exact durable commit, including Fork seeds.
func (st *Store) PrepareConfirmed(id string, c core.Commit, receipt core.Receipt) (Change, error) {
	d := st.Sessions[id]
	if d == nil {
		d = NewData()
	}
	if c.ExpectedVersion != d.State.Version {
		return Change{}, core.ErrConflict
	}
	validRange := len(c.Entries) == 0 && receipt.FirstSequence == 0 && receipt.LastSequence == 0 || len(c.Entries) > 0 && receipt.FirstSequence > 0 && receipt.LastSequence >= receipt.FirstSequence && receipt.LastSequence-receipt.FirstSequence == uint64(len(c.Entries)-1)
	if !validRange || len(c.Entries)+len(c.Attempts) == 0 || receipt.Version != c.ExpectedVersion+1 || receipt.CommitID != c.ID {
		return Change{}, core.ErrCorrupt
	}
	for i, e := range c.Entries {
		if e.Kind == core.EntryInputQueued {
			if _, exists := d.Inputs[e.Input.ID]; exists {
				return Change{}, core.ErrConflict
			}
		}
		if e.Sequence != receipt.FirstSequence+uint64(i) {
			return Change{}, core.ErrCorrupt
		}
	}
	digest, err := c.Digest()
	if err != nil {
		return Change{}, err
	}
	if c.ID == core.ForkCommitID {
		if c.Entries[0].Created == nil {
			return Change{}, core.ErrCorrupt
		}
		digest = c.Entries[0].Created.ForkDigest
	}
	if digest != receipt.Digest {
		return Change{}, core.ErrCorrupt
	}
	state, records, err := core.Reduce(d.State, c)
	if err != nil {
		return Change{}, err
	}
	if state.SessionID != id {
		return Change{}, core.ErrCorrupt
	}
	r := st.Rollouts[state.RolloutID]
	if r == nil {
		r = NewRollout(state.RolloutID)
	}
	validRollout := r.Status == "active" || r.Status == "building" && c.ID == core.ForkCommitID && d.State.Version == 0
	if !validRollout || len(c.Entries) > 0 && receipt.FirstSequence != r.Sequence+1 {
		return Change{}, core.ErrCorrupt
	}
	if len(c.Entries) > 0 && c.Entries[0].Created != nil && c.Entries[0].Created.Projection != nil {
		head, ok := r.Records[c.Entries[0].Created.ForkEntryID]
		if !ok || !Equal(head.Projection, c.Entries[0].Created.Projection) {
			return Change{}, core.ErrCorrupt
		}
	}
	for _, e := range c.Entries {
		if _, ok := r.Records[e.ID]; ok {
			return Change{}, core.ErrCorrupt
		}
		if e.Compaction != nil {
			rec, ok := r.Records[e.Compaction.ThroughID]
			if !ok || rec.Projection == nil {
				return Change{}, core.ErrCorrupt
			}
		}
	}
	if err := pending.Validate(c.Entries, func(id string) (core.InputKind, bool, error) {
		r, ok := d.Pending["input:"+id]
		if !ok {
			return "", false, nil
		}
		return r.Entry.Input.Kind, true, nil
	}); err != nil {
		return Change{}, err
	}

	var attempts []core.ModelAttempt
	seenAttempts := map[string]bool{}
	for _, mutation := range c.Attempts {
		if seenAttempts[mutation.ID] {
			return Change{}, core.ErrCorrupt
		}
		seenAttempts[mutation.ID] = true
		var old *core.ModelAttempt
		if value, exists := d.Attempts[mutation.ID]; exists {
			old = &value
		}
		attempt, err := core.ApplyAttemptMutation(old, mutation, c.Entries)
		if err != nil || attempt.SessionID != id {
			if err == nil {
				err = core.ErrCorrupt
			}
			return Change{}, err
		}
		if mutation.Create != nil {
			head, exists := r.Records[attempt.Input.HeadID]
			for _, record := range records {
				if record.Entry.ID == attempt.Input.HeadID {
					head, exists = record, true
				}
			}
			if !exists || head.Projection == nil || !head.Projection.Complete() || head.Projection.ConfigurationID != attempt.Input.ConfigurationID {
				return Change{}, core.ErrCorrupt
			}
		}
		attempts = append(attempts, attempt)
	}
	return Change{Commit: c, State: state, Records: records, Receipt: receipt, Attempts: attempts}, nil
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
	for _, attempt := range change.Attempts {
		if _, exists := d.Attempts[attempt.ID]; !exists {
			d.AttemptOrder = append(d.AttemptOrder, attempt.ID)
		}
		d.Attempts[attempt.ID] = attempt
	}
	d.State = change.State
	d.Receipts[change.Receipt.CommitID] = change.Receipt
}
func (st *Store) Commit(ctx context.Context, id string, c core.Commit) (core.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return core.Receipt{}, err
	}
	st.Mu.Lock()
	defer st.Mu.Unlock()
	change, err := st.Prepare(id, c)
	if err != nil {
		return core.Receipt{}, err
	}
	if err = ctx.Err(); err != nil {
		return core.Receipt{}, err
	}
	if len(change.Records)+len(change.Attempts) > 0 {
		st.Apply(change)
	}
	return change.Receipt, nil
}

func (st *Store) GetAttempt(ctx context.Context, id, key string) (core.ModelAttempt, error) {
	if err := ctx.Err(); err != nil {
		return core.ModelAttempt{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil {
		return core.ModelAttempt{}, core.ErrNotFound
	}
	a, exists := d.Attempts[key]
	if !exists {
		return core.ModelAttempt{}, core.ErrNotFound
	}
	return Clone(a)
}

func (st *Store) ScanAttempts(ctx context.Context, id string, q core.AttemptQuery) (core.AttemptPage, error) {
	if err := ctx.Err(); err != nil {
		return core.AttemptPage{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil {
		return core.AttemptPage{}, core.ErrNotFound
	}
	var cursor uint64
	var err error
	if q.Cursor != "" {
		cursor, err = strconv.ParseUint(q.Cursor, 10, 63)
		if err != nil {
			return core.AttemptPage{}, core.ErrInvalid
		}
	}
	start, step := 0, 1
	if q.Reverse {
		start, step = len(d.AttemptOrder)-1, -1
	}
	if q.Cursor != "" {
		if q.Reverse {
			start = sort.Search(len(d.AttemptOrder), func(i int) bool { return d.Attempts[d.AttemptOrder[i]].Version >= cursor }) - 1
		} else {
			start = sort.Search(len(d.AttemptOrder), func(i int) bool { return d.Attempts[d.AttemptOrder[i]].Version > cursor })
		}
	}
	limit, maxBytes := core.PageLimits(q.Limit, q.MaxBytes)
	page, size := core.AttemptPage{}, 0
	for i := start; i >= 0 && i < len(d.AttemptOrder); i += step {
		if err := ctx.Err(); err != nil {
			return core.AttemptPage{}, err
		}
		a := d.Attempts[d.AttemptOrder[i]]
		if q.Purpose != "" && a.Purpose != q.Purpose {
			continue
		}
		raw, err := json.Marshal(a)
		if err != nil {
			return core.AttemptPage{}, err
		}
		if len(page.Attempts) == limit || size+len(raw) > maxBytes {
			if len(page.Attempts) == 0 {
				return core.AttemptPage{}, core.ErrBudget
			}
			page.Next = strconv.FormatUint(page.Attempts[len(page.Attempts)-1].Version, 10)
			break
		}
		page.Attempts = append(page.Attempts, a)
		size += len(raw)
	}
	return Clone(page)
}
func (st *Store) Scan(ctx context.Context, id string, q core.Query) (core.Page, error) {
	if err := ctx.Err(); err != nil {
		return core.Page{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Rollouts[id]
	if d == nil || d.Status != "active" {
		return core.Page{}, core.ErrNotFound
	}
	limit, maxBytes := core.PageLimits(q.Limit, q.MaxBytes)
	page := core.Page{}
	size := 0
	add := func(r core.Record) (bool, error) {
		b, err := json.Marshal(r)
		if err != nil {
			return false, err
		}
		if size+len(b) > maxBytes {
			if len(page.Records) == 0 {
				return false, &core.BudgetError{Input: len(b), Window: maxBytes}
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
				return core.Page{}, err
			}
			r, ok := d.Records[key]
			if !ok || !r.Entry.IsHistory() {
				if first {
					return core.Page{}, core.ErrNotFound
				}
				return core.Page{}, core.ErrCorrupt
			}
			first = false
			ok, err := add(r)
			if err != nil {
				return core.Page{}, err
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
				return core.Page{}, core.ErrInvalid
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
				return core.Page{}, err
			}
			r := d.Records[ids[i]]
			if q.SessionID != "" && r.Entry.SessionID != q.SessionID || q.Kind != "" && r.Entry.Kind != q.Kind || q.Reference != "" && core.Reference(r.Entry) != q.Reference || q.HistoryOnly && !r.Entry.IsHistory() {
				continue
			}
			if len(page.Records) == limit {
				page.Next = strconv.FormatUint(page.Records[len(page.Records)-1].Entry.Sequence, 10)
				break
			}
			ok, err := add(r)
			if err != nil {
				return core.Page{}, err
			}
			if !ok {
				page.Next = strconv.FormatUint(page.Records[len(page.Records)-1].Entry.Sequence, 10)
				break
			}
		}
	}
	return Clone(page)
}
func (st *Store) Pending(ctx context.Context, id string, q core.PendingQuery) (core.Page, error) {
	if err := ctx.Err(); err != nil {
		return core.Page{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil || st.Rollouts[d.State.RolloutID] == nil || st.Rollouts[d.State.RolloutID].Status != "active" {
		return core.Page{}, core.ErrNotFound
	}
	var rows []core.Record
	for _, r := range d.Pending {
		if q.InputID != "" && (r.Entry.Input == nil || r.Entry.Input.ID != q.InputID) {
			continue
		}
		if q.Kind != "" && r.Entry.Kind != q.Kind && (q.Kind != core.EntryInputQueued || r.Entry.Kind != core.EntryInputRequeued) {
			continue
		}
		if q.InputKind != "" && (r.Entry.Input == nil || r.Entry.Input.Kind != q.InputKind) {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Entry.Sequence < rows[j].Entry.Sequence })
	limit, maxBytes := core.PageLimits(q.Limit, q.MaxBytes)
	cursor := uint64(0)
	if q.Cursor != "" {
		var err error
		cursor, err = strconv.ParseUint(q.Cursor, 10, 64)
		if err != nil {
			return core.Page{}, core.ErrInvalid
		}
	}
	page := core.Page{}
	size := 0
	for _, r := range rows {
		if r.Entry.Sequence <= cursor {
			continue
		}
		b, _ := json.Marshal(r)
		if len(page.Records) == limit || size+len(b) > maxBytes {
			if len(page.Records) == 0 {
				return core.Page{}, core.ErrBudget
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
func (st *Store) ExistingFork(r core.ForkRequest) (core.RuntimeState, bool, error) {
	d := st.Sessions[r.TargetID]
	if d == nil {
		return core.RuntimeState{}, false, nil
	}
	if rollout := st.Rollouts[d.State.RolloutID]; rollout == nil || rollout.Status != "active" {
		return core.RuntimeState{}, true, core.ErrNotFound
	}
	digest, err := r.Digest()
	if err != nil {
		return core.RuntimeState{}, true, err
	}
	receipt, ok := d.Receipts[core.ForkCommitID]
	if !ok || receipt.Digest != digest {
		return core.RuntimeState{}, true, core.ErrConflict
	}
	state, err := Clone(d.State)
	return state, true, err
}
func (st *Store) Fork(ctx context.Context, r core.ForkRequest) (core.RuntimeState, error) {
	if err := core.ValidateFork(r); err != nil {
		return core.RuntimeState{}, err
	}
	unlock, err := st.LockFork(ctx, r.TargetID)
	if err != nil {
		return core.RuntimeState{}, err
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
		return state, core.ErrNotFound
	}
	head, err := st.Get(ctx, r.SourceRolloutID, r.HeadID)
	if err != nil {
		return state, err
	}
	if _, err = core.ForkState(r, head); err != nil {
		return state, err
	}
	var detached *Rollout
	if r.Mode == core.ForkDetached {
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
		return state, core.ErrNotFound
	}
	if detached != nil {
		if st.Rollouts[detached.ID] != nil {
			return state, core.ErrConflict
		}
		rollout = detached
	}
	c, state, records, receipt, err := core.PrepareFork(r, head, rollout.Sequence+1)
	if err != nil {
		return state, err
	}
	if detached != nil {
		st.Rollouts[detached.ID] = detached
	}
	st.Apply(Change{Commit: c, State: state, Records: records, Receipt: receipt})
	return Clone(state)
}

var _ core.Store = (*Store)(nil)

func (st *Store) LookupInput(ctx context.Context, id, key string) (core.InputReceipt, error) {
	if err := ctx.Err(); err != nil {
		return core.InputReceipt{}, err
	}
	st.Mu.RLock()
	defer st.Mu.RUnlock()
	d := st.Sessions[id]
	if d == nil {
		return core.InputReceipt{}, core.ErrNotFound
	}
	receipt, exists := d.Inputs[key]
	if !exists {
		return core.InputReceipt{}, core.ErrNotFound
	}
	return Clone(receipt)
}
