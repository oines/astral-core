// Package jsonl is a durable version-2 checksummed journal. It rebuilds its
// in-memory indexes once on open and thereafter reads only newly appended data.
// The host must give one process exclusive directory ownership. Use SQLite for
// bounded-memory, indexed long-history storage.
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
	"sync"

	core "github.com/oines/astral-core"
	s "github.com/oines/astral-core/session"
	"github.com/oines/astral-core/store/internal/ledger"
)

type cached struct {
	file   *os.File
	offset int64
	header bool
	dirty  bool
}
type directory struct {
	mu       sync.Mutex
	index    *ledger.Store
	files    map[string]*cached
	reserved map[string]bool
	refs     int
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
	Format    int    `json:"format_version"`
	SessionID string `json:"session_id"`
}
type envelope struct {
	Body   json.RawMessage `json:"body"`
	SHA256 string          `json:"sha256"`
}
type journal struct {
	Commit  *s.Commit       `json:"commit,omitempty"`
	Records []s.Record      `json:"fork_records,omitempty"`
	Publish *s.RuntimeState `json:"publish,omitempty"`
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
		d = &directory{index: ledger.New(), files: map[string]*cached{}, reserved: map[string]bool{}}
		directories.all[abs] = d
	}
	d.refs++
	return &Store{directory: abs, shared: d}, nil
}
func (st *Store) filename(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(st.directory, hex.EncodeToString(sum[:])+".jsonl")
}
func (st *Store) syncDirectory() error {
	f, err := os.Open(st.directory)
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
	d.files = nil
	d.index = ledger.New()
	delete(directories.all, st.directory)
	return errors.Join(errs...)
}
func (st *Store) refresh(ctx context.Context, id string) error {
	if st.closed {
		return core.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d := st.shared
	if d.reserved[id] {
		return s.ErrNotFound
	}
	c := d.files[id]
	if c == nil {
		f, err := os.OpenFile(st.filename(id), os.O_RDWR, 0600)
		if errors.Is(err, os.ErrNotExist) {
			return s.ErrNotFound
		}
		if err != nil {
			return err
		}
		c = &cached{file: f}
		d.files[id] = c
	}
	info, err := c.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < c.offset {
		return s.ErrCorrupt
	}
	reader := bufio.NewReader(io.NewSectionReader(c.file, c.offset, info.Size()-c.offset))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !c.header {
			var h header
			if json.Unmarshal(line, &h) != nil || h.Format != s.FormatVersion {
				return s.ErrUnsupportedFormat
			}
			if h.SessionID != id {
				return s.ErrCorrupt
			}
			c.header = true
			c.offset += int64(len(line))
			continue
		}
		var e envelope
		if json.Unmarshal(line, &e) != nil {
			return s.ErrCorrupt
		}
		hash := sha256.Sum256(e.Body)
		if e.SHA256 != hex.EncodeToString(hash[:]) {
			return s.ErrCorrupt
		}
		var j journal
		if json.Unmarshal(e.Body, &j) != nil {
			return s.ErrCorrupt
		}
		index := d.index
		index.Mu.Lock()
		data := index.Sessions[id]
		if data == nil {
			data = ledger.NewData()
			index.Sessions[id] = data
		}
		switch {
		case j.Commit != nil:
			next, records, receipt, prepareErr := data.Prepare(id, *j.Commit)
			if prepareErr != nil {
				index.Mu.Unlock()
				return prepareErr
			}
			if len(records) > 0 {
				data.Apply(next, records, receipt)
			}
		case len(j.Records) > 0:
			if data.Ready {
				index.Mu.Unlock()
				return s.ErrCorrupt
			}
			for _, r := range j.Records {
				if _, exists := data.Records[r.Entry.ID]; exists || r.Entry.ID == "" {
					index.Mu.Unlock()
					return s.ErrCorrupt
				}
				data.Add(r)
			}
		case j.Publish != nil:
			if data.Ready || j.Publish.SessionID != id || j.Publish.FormatVersion != s.FormatVersion {
				index.Mu.Unlock()
				return s.ErrCorrupt
			}
			if _, ok := data.Records[j.Publish.Branch.HeadID]; !ok {
				index.Mu.Unlock()
				return s.ErrCorrupt
			}
			data.State = *j.Publish
			data.Sort()
			data.Ready = true
		default:
			index.Mu.Unlock()
			return s.ErrCorrupt
		}
		index.Mu.Unlock()
		c.offset += int64(len(line))
		c.dirty = true
	}
	if !c.header && info.Size() > 0 {
		return s.ErrUnsupportedFormat
	}
	if c.dirty {
		if err = c.file.Sync(); err != nil {
			return err
		}
		if err = st.syncDirectory(); err != nil {
			return err
		}
		c.dirty = false
	}
	return nil
}
func (st *Store) LoadState(ctx context.Context, id string) (s.RuntimeState, error) {
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err := st.refresh(ctx, id); err != nil {
		return s.RuntimeState{}, err
	}
	return st.shared.index.LoadState(ctx, id)
}
func (st *Store) Get(ctx context.Context, id, key string) (s.Record, error) {
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err := st.refresh(ctx, id); err != nil {
		return s.Record{}, err
	}
	return st.shared.index.Get(ctx, id, key)
}
func (st *Store) Scan(ctx context.Context, id string, q s.Query) (s.Page, error) {
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err := st.refresh(ctx, id); err != nil {
		return s.Page{}, err
	}
	return st.shared.index.Scan(ctx, id, q)
}
func (st *Store) Pending(ctx context.Context, id string, q s.PendingQuery) (s.Page, error) {
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err := st.refresh(ctx, id); err != nil {
		return s.Page{}, err
	}
	return st.shared.index.Pending(ctx, id, q)
}
func (st *Store) LookupCommit(ctx context.Context, id, key string) (s.Receipt, error) {
	st.shared.mu.Lock()
	defer st.shared.mu.Unlock()
	if err := st.refresh(ctx, id); err != nil {
		return s.Receipt{}, err
	}
	return st.shared.index.LookupCommit(ctx, id, key)
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
func (st *Store) Commit(ctx context.Context, id string, commit s.Commit) (s.Receipt, error) {
	if id == "" {
		return s.Receipt{}, core.ErrInvalid
	}
	d := st.shared
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reserved[id] {
		return s.Receipt{}, s.ErrConflict
	}
	if err := st.refresh(ctx, id); err != nil && !errors.Is(err, s.ErrNotFound) {
		return s.Receipt{}, err
	}
	d.index.Mu.Lock()
	defer d.index.Mu.Unlock()
	data := d.index.Sessions[id]
	if data == nil {
		data = ledger.NewData()
	}
	state, records, receipt, err := data.Prepare(id, commit)
	if err != nil {
		return s.Receipt{}, err
	}
	if len(records) == 0 {
		return receipt, nil
	}
	line, err := encode(journal{Commit: &commit})
	if err != nil {
		return s.Receipt{}, err
	}
	c := d.files[id]
	if c == nil {
		f, err := os.OpenFile(st.filename(id), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return s.Receipt{}, err
		}
		c = &cached{file: f}
		d.files[id] = c
	}
	if err = ctx.Err(); err != nil {
		return s.Receipt{}, err
	}
	if !c.header {
		h, _ := json.Marshal(header{Format: s.FormatVersion, SessionID: id})
		h = append(h, '\n')
		if err = writeAll(c.file, h); err != nil {
			return s.Receipt{}, err
		}
		c.header = true
		c.offset = int64(len(h))
	}
	if err = c.file.Truncate(c.offset); err != nil {
		return s.Receipt{}, err
	}
	if _, err = c.file.Seek(c.offset, io.SeekStart); err != nil {
		return s.Receipt{}, err
	}
	if err = writeAll(c.file, line); err != nil {
		return s.Receipt{}, err
	}
	// Leave the offset unchanged until sync succeeds. LookupCommit can then read
	// and sync this exact stable commit after an ambiguous acknowledgement.
	if err = c.file.Sync(); err != nil {
		return s.Receipt{}, err
	}
	if err = st.syncDirectory(); err != nil {
		return s.Receipt{}, err
	}
	c.offset += int64(len(line))
	data.Apply(state, records, receipt)
	d.index.Sessions[id] = data
	return receipt, nil
}
func (st *Store) Fork(ctx context.Context, r s.ForkRequest) (state s.RuntimeState, err error) {
	if err = s.ValidateFork(r); err != nil {
		return state, err
	}
	d := st.shared
	d.mu.Lock()
	if st.closed {
		d.mu.Unlock()
		return state, core.ErrClosed
	}
	if d.reserved[r.TargetID] {
		d.mu.Unlock()
		return state, s.ErrConflict
	}
	if _, statErr := os.Stat(st.filename(r.TargetID)); statErr == nil {
		d.mu.Unlock()
		return state, s.ErrConflict
	} else if !errors.Is(statErr, os.ErrNotExist) {
		d.mu.Unlock()
		return state, statErr
	}
	d.reserved[r.TargetID] = true
	d.mu.Unlock()
	defer func() { d.mu.Lock(); delete(d.reserved, r.TargetID); d.mu.Unlock() }()
	head, err := st.Get(ctx, r.SourceID, r.HeadID)
	if err != nil {
		return state, err
	}
	state, err = s.ForkState(r, head)
	if err != nil {
		return state, err
	}
	file, err := os.CreateTemp(st.directory, ".fork-")
	if err != nil {
		return state, err
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	h, _ := json.Marshal(header{Format: s.FormatVersion, SessionID: r.TargetID})
	if err = writeAll(file, append(h, '\n')); err != nil {
		return state, err
	}
	emit := func(records []s.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := encode(journal{Records: records})
		if err != nil {
			return err
		}
		return writeAll(file, line)
	}
	if err = emit([]s.Record{{Entry: r.Creation}}); err != nil {
		return state, err
	}
	cursor := ""
	for {
		page, readErr := st.Scan(ctx, r.SourceID, s.Query{PathHead: r.HeadID, Cursor: cursor, Limit: s.DefaultPageSize, MaxBytes: s.DefaultContextBytes})
		if readErr != nil {
			return state, readErr
		}
		for i := range page.Records {
			page.Records[i].Entry.Version = 1
		}
		if err = emit(page.Records); err != nil {
			return state, err
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if r.Environment != nil {
		next, records, reduceErr := s.Reduce(state, s.Commit{ID: r.Environment.ID, ExpectedVersion: state.Version, Entries: []s.Entry{*r.Environment}})
		if reduceErr != nil {
			return state, reduceErr
		}
		state = next
		if err = emit(records); err != nil {
			return state, err
		}
	}
	line, err := encode(journal{Publish: &state})
	if err != nil {
		return state, err
	}
	if err = writeAll(file, line); err != nil {
		return state, err
	}
	if err = file.Sync(); err != nil {
		return state, err
	}
	if err = ctx.Err(); err != nil {
		return state, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if st.closed {
		return state, core.ErrClosed
	}
	if err = os.Link(name, st.filename(r.TargetID)); err != nil {
		return state, fmt.Errorf("publish fork: %w", err)
	}
	if err = st.syncDirectory(); err != nil {
		return state, err
	}
	return state, nil
}

var _ s.Store = (*Store)(nil)
