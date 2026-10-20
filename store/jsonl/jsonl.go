// Package jsonl persists one checksummed journal per Rollout. A single process
// owns the directory; handles in that process share indexes and write locks.
// Cold replay and indexes grow with history. Use SQLite for bounded storage reads.
package jsonl

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	core "github.com/oines/astral-core"
	s "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/internal/historypage"
	"github.com/oines/astral-core/store/internal/ledger"
)

type cached struct {
	file   *os.File
	offset int64
	header bool
}
type directory struct {
	mu    sync.Mutex
	index *ledger.Store
	files map[string]*cached
	refs  int
	dirty bool // only unresolved writes require discovery/replay after initial Open
}

var directories = struct {
	sync.Mutex
	all map[string]*directory
}{all: map[string]*directory{}}

type Store struct {
	directory string
	shared    *directory
	closed    bool
}
type header struct {
	Format    int            `json:"format_version"`
	RolloutID string         `json:"rollout_id"`
	Fork      *s.ForkRequest `json:"fork,omitempty"`
}
type envelope struct {
	Body   json.RawMessage `json:"body"`
	SHA256 string          `json:"sha256"`
}
type journal struct {
	SessionID string     `json:"session_id,omitempty"`
	Commit    *s.Commit  `json:"commit,omitempty"`
	Receipt   *s.Receipt `json:"receipt,omitempty"`
	Records   []s.Record `json:"import,omitempty"`
}

func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	directories.Lock()
	defer directories.Unlock()
	d := directories.all[abs]
	if d == nil {
		if err = checkFormat(abs); err != nil {
			return nil, err
		}
		d = &directory{index: ledger.New(), files: map[string]*cached{}, dirty: true}
		st := &Store{directory: abs, shared: d}
		if err = st.refreshAll(context.Background()); err != nil {
			for _, c := range d.files {
				c.file.Close()
			}
			return nil, err
		}
		directories.all[abs] = d
	}
	d.refs++
	return &Store{directory: abs, shared: d}, nil
}
func checkFormat(dir string) error {
	name := filepath.Join(dir, "format.json")
	b, err := os.ReadFile(name)
	if err == nil {
		var h header
		if json.Unmarshal(b, &h) != nil || !s.SupportsFormat(h.Format) {
			return s.ErrUnsupportedFormat
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if !strings.HasPrefix(f.Name(), ".format-") {
			return s.ErrUnsupportedFormat
		}
	}
	f, err := os.CreateTemp(dir, ".format-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	b, _ = json.Marshal(header{Format: s.FormatVersion})
	if err = writeAll(f, b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = os.Link(f.Name(), name); err != nil {
		return err
	}
	return syncDir(dir)
}
func (st *Store) filename(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(st.directory, hex.EncodeToString(sum[:])+".jsonl")
}
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (st *Store) Close() error {
	directories.Lock()
	defer directories.Unlock()
	d := st.shared
	d.mu.Lock()
	defer d.mu.Unlock()
	if st.closed {
		return nil
	}
	st.closed = true
	d.refs--
	if d.refs > 0 {
		return nil
	}
	var errs []error
	for _, c := range d.files {
		errs = append(errs, c.file.Close())
	}
	delete(directories.all, st.directory)
	return errors.Join(errs...)
}

// refreshAll also resolves complete but unacknowledged writes before any read.
func (st *Store) refreshAll(ctx context.Context) error {
	if st.closed {
		return core.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !st.shared.dirty {
		return nil
	}
	files, err := os.ReadDir(st.directory)
	if err != nil {
		return err
	}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".jsonl") {
			continue
		}
		name := filepath.Join(st.directory, file.Name())
		var c *cached
		var id string
		for key, value := range st.shared.files {
			if st.filename(key) == name {
				id = key
				c = value
				break
			}
		}
		if c == nil {
			f, e := os.OpenFile(name, os.O_RDWR, 0600)
			if e != nil {
				return e
			}
			line, e := bufio.NewReader(f).ReadBytes('\n')
			var h header
			if e != nil || json.Unmarshal(line, &h) != nil || !s.SupportsFormat(h.Format) {
				f.Close()
				return s.ErrUnsupportedFormat
			}
			if h.RolloutID == "" || st.filename(h.RolloutID) != name {
				f.Close()
				return s.ErrCorrupt
			}
			id = h.RolloutID
			c = &cached{file: f}
			st.shared.files[id] = c
		}
		if err = st.refresh(ctx, id, c); err != nil {
			return err
		}
	}
	st.shared.dirty = false
	return nil
}
func (st *Store) refresh(ctx context.Context, id string, c *cached) error {
	info, err := c.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < c.offset {
		return s.ErrCorrupt
	}
	if info.Size() == c.offset {
		return nil
	}
	// A previous Sync/ack may have failed. Do not expose those bytes until synced.
	if err = c.file.Sync(); err != nil {
		return err
	}
	if err = syncDir(st.directory); err != nil {
		return err
	}
	reader := bufio.NewReader(io.NewSectionReader(c.file, c.offset, info.Size()-c.offset))
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		line, e := reader.ReadBytes('\n')
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if !c.header {
			var h header
			if json.Unmarshal(line, &h) != nil || !s.SupportsFormat(h.Format) {
				return s.ErrUnsupportedFormat
			}
			if h.RolloutID != id {
				return s.ErrCorrupt
			}
			c.header = true
			c.offset += int64(len(line))
			continue
		}
		var env envelope
		if json.Unmarshal(line, &env) != nil {
			return s.ErrCorrupt
		}
		hash := sha256.Sum256(env.Body)
		if env.SHA256 != hex.EncodeToString(hash[:]) {
			return s.ErrCorrupt
		}
		var j journal
		if json.Unmarshal(env.Body, &j) != nil {
			return s.ErrCorrupt
		}
		index := st.shared.index
		index.Mu.Lock()
		err = applyJournal(index, id, j)
		index.Mu.Unlock()
		if err != nil {
			return err
		}
		c.offset += int64(len(line))
	}
	return nil
}
func applyJournal(index *ledger.Store, id string, j journal) error {
	if len(j.Records) > 0 {
		if j.Commit != nil || j.Receipt != nil {
			return s.ErrCorrupt
		}
		r := index.Rollouts[id]
		if r == nil {
			r = ledger.NewRollout(id)
			r.Status = "building"
			index.Rollouts[id] = r
		}
		if r.Status != "building" {
			return s.ErrCorrupt
		}
		for _, record := range j.Records {
			if !record.Entry.IsHistory() || record.Projection == nil || record.Entry.ID == "" || record.Entry.Sequence == 0 || record.Entry.Version == 0 || record.Entry.SessionID == "" || record.Projection.HeadID != record.Entry.ID {
				return s.ErrCorrupt
			}
			if _, exists := r.Records[record.Entry.ID]; exists {
				return s.ErrCorrupt
			}
			if len(r.Order) > 0 {
				previous := r.Records[r.Order[len(r.Order)-1]].Entry
				if previous.ParentID != record.Entry.ID || previous.Sequence <= record.Entry.Sequence {
					return s.ErrCorrupt
				}
			}
			r.Add(record)
		}
		return nil
	}
	if j.Commit == nil || j.Receipt == nil || j.SessionID == "" {
		return s.ErrCorrupt
	}
	if len(j.Commit.Entries) == 0 {
		return s.ErrCorrupt
	}
	if created := j.Commit.Entries[0].Created; created != nil && created.RolloutID != id {
		return s.ErrCorrupt
	}
	if d := index.Sessions[j.SessionID]; d != nil && d.State.RolloutID != id {
		return s.ErrCorrupt
	}
	if r := index.Rollouts[id]; r != nil && r.Status == "building" {
		if len(r.Order) == 0 || r.Records[r.Order[len(r.Order)-1]].Entry.ParentID != "" {
			return s.ErrCorrupt
		}
	}
	change, err := index.PrepareConfirmed(j.SessionID, *j.Commit, *j.Receipt)
	if err != nil {
		return err
	}
	index.Apply(change)
	if j.Commit.ID == s.ForkCommitID {
		index.Rollouts[id].Sort()
	}
	return nil
}
func encode(j journal) ([]byte, error) {
	body, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	line, err := json.Marshal(envelope{Body: body, SHA256: hex.EncodeToString(sum[:])})
	return append(line, '\n'), err
}
func writeAll(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}
func (st *Store) append(ctx context.Context, id string, j journal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	line, err := encode(j)
	if err != nil {
		return err
	}
	st.shared.dirty = true
	c := st.shared.files[id]
	if c == nil {
		f, e := os.CreateTemp(st.directory, filepath.Base(st.filename(id))+".create-")
		if e != nil {
			return e
		}
		keep := false
		defer func() {
			os.Remove(f.Name())
			if !keep {
				f.Close()
			}
		}()
		h, _ := json.Marshal(header{Format: s.FormatVersion, RolloutID: id})
		h = append(h, '\n')
		if e = writeAll(f, h); e == nil {
			e = writeAll(f, line)
		}
		if e == nil {
			e = f.Sync()
		}
		if e == nil {
			e = os.Link(f.Name(), st.filename(id))
		}
		if e == nil {
			e = syncDir(st.directory)
		}
		if e != nil {
			return e
		}
		st.shared.files[id] = &cached{file: f, offset: int64(len(h) + len(line)), header: true}
		keep = true
		st.shared.dirty = false
		return nil
	}
	if err = c.file.Truncate(c.offset); err != nil {
		return err
	}
	if _, err = c.file.Seek(c.offset, io.SeekStart); err != nil {
		return err
	}
	if err = writeAll(c.file, line); err != nil {
		return err
	}
	if err = c.file.Sync(); err != nil {
		return err
	}
	c.offset += int64(len(line))
	st.shared.dirty = false
	return nil
}
func (st *Store) LoadState(ctx context.Context, id string) (s.RuntimeState, error) {
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err := st.refreshAll(ctx); err != nil {
		return s.RuntimeState{}, err
	}
	return st.shared.index.LoadState(ctx, id)
}
func (st *Store) Get(ctx context.Context, id, key string) (s.Record, error) {
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err := st.refreshAll(ctx); err != nil {
		return s.Record{}, err
	}
	return st.shared.index.Get(ctx, id, key)
}
func (st *Store) Scan(ctx context.Context, id string, q s.Query) (s.Page, error) {
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err := st.refreshAll(ctx); err != nil {
		return s.Page{}, err
	}
	return st.shared.index.Scan(ctx, id, q)
}
func (st *Store) Pending(ctx context.Context, id string, q s.PendingQuery) (s.Page, error) {
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err := st.refreshAll(ctx); err != nil {
		return s.Page{}, err
	}
	return st.shared.index.Pending(ctx, id, q)
}
func (st *Store) LookupCommit(ctx context.Context, id, key string) (s.Receipt, error) {
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err := st.refreshAll(ctx); err != nil {
		return s.Receipt{}, err
	}
	return st.shared.index.LookupCommit(ctx, id, key)
}
func (st *Store) Commit(ctx context.Context, id string, c s.Commit) (s.Receipt, error) {
	d := st.shared
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := st.refreshAll(ctx); err != nil {
		return s.Receipt{}, err
	}
	d.index.Mu.Lock()
	defer d.index.Mu.Unlock()
	change, err := d.index.Prepare(id, c)
	if err != nil {
		return s.Receipt{}, err
	}
	if len(change.Records) == 0 {
		return change.Receipt, nil
	}
	if err = st.append(ctx, change.State.RolloutID, journal{SessionID: id, Commit: &change.Commit, Receipt: &change.Receipt}); err != nil {
		return s.Receipt{}, err
	}
	d.index.Apply(change)
	return change.Receipt, nil
}
func (st *Store) Fork(ctx context.Context, r s.ForkRequest) (s.RuntimeState, error) {
	return st.fork(ctx, r, nil)
}
func (st *Store) fork(ctx context.Context, r s.ForkRequest, afterPage func()) (s.RuntimeState, error) {
	return st.forkWithCheckpoint(ctx, r, func(phase string) {
		if phase == "page" && afterPage != nil {
			afterPage()
		}
	})
}

// The private checkpoint observes durable boundaries in real process-crash tests.
func (st *Store) forkWithCheckpoint(ctx context.Context, r s.ForkRequest, checkpoint func(string)) (state s.RuntimeState, err error) {
	if err = s.ValidateFork(r); err != nil {
		return state, err
	}
	d := st.shared
	unlock, err := d.index.LockFork(ctx, r.TargetID)
	if err != nil {
		return state, err
	}
	defer unlock()
	d.mu.Lock()
	if err = st.refreshAll(ctx); err != nil {
		d.mu.Unlock()
		return state, err
	}
	d.index.Mu.RLock()
	state, found, err := d.index.ExistingFork(r)
	d.index.Mu.RUnlock()
	if found || err != nil {
		d.mu.Unlock()
		return state, err
	}
	source, err := d.index.LoadState(ctx, r.SourceID)
	if err != nil || source.RolloutID != r.SourceRolloutID {
		d.mu.Unlock()
		if err != nil {
			return state, err
		}
		return state, s.ErrNotFound
	}
	head, err := d.index.Get(ctx, r.SourceRolloutID, r.HeadID)
	if err != nil {
		d.mu.Unlock()
		return state, err
	}
	if _, err = s.ForkState(r, head); err != nil {
		d.mu.Unlock()
		return state, err
	}
	if r.Mode == s.ForkShared {
		defer d.mu.Unlock()
		d.index.Mu.Lock()
		defer d.index.Mu.Unlock()
		rollout := d.index.Rollouts[r.SourceRolloutID]
		c, next, records, receipt, e := s.PrepareFork(r, head, rollout.Sequence+1)
		if e != nil {
			return state, e
		}
		if e = st.append(ctx, r.RolloutID(), journal{SessionID: r.TargetID, Commit: &c, Receipt: &receipt}); e != nil {
			return state, e
		}
		d.index.Apply(ledger.Change{Commit: c, State: next, Records: records, Receipt: receipt})
		return ledger.Clone(next)
	}
	pending := st.filename(r.RolloutID()) + ".pending"
	if data, e := os.Open(pending); e == nil {
		line, _ := bufio.NewReader(data).ReadBytes('\n')
		data.Close()
		var h header
		if json.Unmarshal(line, &h) != nil || h.Fork == nil || h.RolloutID != r.RolloutID() {
			d.mu.Unlock()
			return state, s.ErrCorrupt
		}
		{
			a, _ := h.Fork.Digest()
			b, _ := r.Digest()
			if a != b {
				d.mu.Unlock()
				return state, s.ErrConflict
			}
		}
		if e = os.Remove(pending); e != nil {
			d.mu.Unlock()
			return state, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		d.mu.Unlock()
		return state, e
	}
	if err = st.removePreparing(r.RolloutID()); err != nil {
		d.mu.Unlock()
		return state, err
	}
	file, err := st.createPending(r, pending)
	d.mu.Unlock()
	if err != nil {
		return state, err
	}
	defer file.Close()
	defer os.Remove(pending)

	imported := ledger.NewRollout(r.RolloutID())
	cursor := ""
	for {
		page, e := historypage.Read(ctx, st, r.SourceRolloutID, r.HeadID, cursor)
		if e != nil {
			return state, e
		}
		line, e := encode(journal{Records: page.Records})
		if e != nil {
			return state, e
		}
		if e = writeAll(file, line); e != nil {
			return state, e
		}
		for _, record := range page.Records {
			imported.Add(record)
		}
		if checkpoint != nil {
			checkpoint("page")
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	imported.Sort()
	c, state, records, receipt, err := s.PrepareFork(r, head, imported.Sequence+1)
	if err != nil {
		return state, err
	}
	line, err := encode(journal{SessionID: r.TargetID, Commit: &c, Receipt: &receipt})
	if err != nil {
		return state, err
	}
	if err = writeAll(file, line); err != nil {
		return state, err
	}
	if err = file.Sync(); err != nil {
		return state, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err = st.refreshAll(ctx); err != nil {
		return state, err
	}
	d.index.Mu.Lock()
	defer d.index.Mu.Unlock()
	if d.index.Sessions[r.TargetID] != nil || d.index.Rollouts[r.RolloutID()] != nil {
		return state, s.ErrConflict
	}
	if parent := d.index.Rollouts[r.SourceRolloutID]; parent == nil || parent.Status != "active" {
		return state, s.ErrNotFound
	}
	if checkpoint != nil {
		checkpoint("before_publish")
	}
	d.dirty = true
	if err = os.Link(pending, st.filename(r.RolloutID())); err != nil {
		return state, fmt.Errorf("publish fork: %w", err)
	}
	if err = syncDir(st.directory); err != nil {
		return state, err
	}
	opened, err := os.OpenFile(st.filename(r.RolloutID()), os.O_RDWR, 0600)
	if err != nil {
		return state, err
	}
	info, err := opened.Stat()
	if err != nil {
		opened.Close()
		return state, err
	}
	d.files[r.RolloutID()] = &cached{file: opened, offset: info.Size(), header: true}
	d.index.Rollouts[imported.ID] = imported
	d.index.Apply(ledger.Change{Commit: c, State: state, Records: records, Receipt: receipt})
	d.dirty = false
	if checkpoint != nil {
		checkpoint("after_publish")
	}
	return ledger.Clone(state)
}

// Publish a complete, synced identity before any copy page is written.
func (st *Store) createPending(r s.ForkRequest, pending string) (*os.File, error) {
	f, err := os.CreateTemp(st.directory, filepath.Base(st.filename(r.RolloutID()))+".preparing-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	h, err := json.Marshal(header{Format: s.FormatVersion, RolloutID: r.RolloutID(), Fork: &r})
	if err == nil {
		err = writeAll(f, append(h, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = os.Link(f.Name(), pending)
	}
	if err == nil {
		err = syncDir(st.directory)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (st *Store) removePreparing(rolloutID string) error {
	names, err := filepath.Glob(st.filename(rolloutID) + ".preparing-*")
	if err != nil {
		return err
	}
	for _, name := range names {
		if err = os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (st *Store) DiscardUnpublished(ctx context.Context, id string) error {
	unlock, err := st.shared.index.LockFork(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err = st.refreshAll(ctx); err != nil {
		return err
	}
	st.shared.index.Mu.RLock()
	exists := st.shared.index.Sessions[id] != nil
	st.shared.index.Mu.RUnlock()
	if exists {
		return s.ErrConflict
	}
	target := s.ForkRequest{TargetID: id, Mode: s.ForkDetached}
	err = os.Remove(st.filename(target.RolloutID()) + ".pending")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = st.removePreparing(target.RolloutID()); err != nil {
		return err
	}
	return syncDir(st.directory)
}

var _ s.Store = (*Store)(nil)
