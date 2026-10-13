package standardtools

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	astral "github.com/oines/astral-core"
)

type terminal struct {
	id           int
	invocation   astral.Invocation
	process      Process
	tty          bool
	interaction  chan struct{}
	lastUsed     time.Time // guarded by Set.mu
	unavailable  bool      // guarded by Set.mu; retained for cleanup after unknown exit
	mu           sync.Mutex
	pending      headTailBuffer
	notify       chan struct{}
	exited       bool
	outputClosed bool
	exitCode     int
	waitErr      error
	readErr      error
}

func (p *terminal) signal() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}
func (p *terminal) release() { p.interaction <- struct{}{} }
func (p *terminal) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return notExecuted(ctx.Err().Error())
	case <-p.interaction:
		if err := ctx.Err(); err != nil {
			p.release()
			return notExecuted(err.Error())
		}
		return nil
	}
}

func (s *Set) lookup(id int, owner string) (*terminal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.processes[id]
	if s.closed || p == nil || p.unavailable || p.invocation.SessionID != owner {
		return nil, notExecuted(fmt.Sprintf("unknown terminal session_id %d for this Session", id))
	}
	p.lastUsed = time.Now()
	return p, nil
}

// Reserve before spawning so concurrent starts cannot exceed the cap. Idle
// terminals may be evicted, but active interactions are never interrupted to
// make room. IDs are not reused during this Set's lifetime.
func (s *Set) reserve() (int, *terminal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, nil, notExecuted("tool set is closed")
	}
	var evicted *terminal
	if len(s.processes)+len(s.reserved) >= s.options.MaxProcesses {
		candidates := make([]*terminal, 0, len(s.processes))
		for _, p := range s.processes {
			candidates = append(candidates, p)
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].lastUsed.Before(candidates[j].lastUsed) })
		for _, p := range candidates {
			select {
			case <-p.interaction:
				evicted = p
			default:
			}
			if evicted != nil {
				break
			}
		}
		if evicted == nil {
			return 0, nil, notExecuted("all terminal sessions are busy; wait for an active interaction to finish")
		}
	}
	// A random starting point plus a non-repeating counter avoids retaining an
	// unbounded map of every command ever run by a long-lived host.
	if s.issuedIDs == 0x7fffffff {
		if evicted != nil {
			evicted.release()
		}
		return 0, nil, notExecuted("terminal session IDs exhausted; create a new tool set")
	}
	if s.nextID == 0 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			if evicted != nil {
				evicted.release()
			}
			return 0, nil, err
		}
		s.nextID = max(1, binary.LittleEndian.Uint32(b[:])&0x7fffffff)
	}
	id := int(s.nextID)
	s.nextID = (s.nextID % 0x7fffffff) + 1
	s.issuedIDs++
	s.reserved[id] = true
	return id, evicted, nil
}

func (s *Set) start(ctx context.Context, inv astral.Invocation, request ProcessRequest) (*terminal, error) {
	var err error
	inv.Environment, err = copyEnvironment(inv.Environment)
	if err != nil {
		return nil, err
	}
	id, evicted, err := s.reserve()
	if err != nil {
		return nil, err
	}
	defer func() { s.mu.Lock(); delete(s.reserved, id); s.mu.Unlock() }()
	if evicted != nil {
		err := terminateProcess(ctx, evicted.process)
		if err == nil {
			s.mu.Lock()
			if s.processes[evicted.id] == evicted {
				delete(s.processes, evicted.id)
			}
			s.mu.Unlock()
		}
		evicted.release()
		if err != nil {
			return nil, notExecuted("could not retire an older terminal: " + err.Error())
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, notExecuted(err.Error())
	}
	process, err := s.backend.StartProcess(ctx, inv, request)
	if err != nil {
		return nil, err
	}
	if process == nil {
		return nil, &astral.ToolError{Outcome: astral.OutcomeUnknown, Code: "invalid_process", Cause: errors.New("backend returned no process after startup")}
	}
	owner := astral.Invocation{SessionID: inv.SessionID, Binding: inv.Binding, Environment: inv.Environment}
	p := &terminal{id: id, invocation: owner, process: process, tty: request.TTY, lastUsed: time.Now(), interaction: make(chan struct{}, 1), pending: headTailBuffer{limit: s.options.MaxBufferedBytes}, notify: make(chan struct{}, 1)}
	s.mu.Lock()
	closed := s.closed
	s.processes[id] = p
	delete(s.reserved, id)
	s.workers.Add(2)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		var readErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				readErr = fmt.Errorf("output reader panic: %v", recovered)
			}
			if readErr != nil {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				readErr = errors.Join(readErr, terminateProcess(cleanup, process))
				cancel()
			}
			p.mu.Lock()
			p.outputClosed, p.readErr = true, readErr
			p.mu.Unlock()
			p.signal()
		}()
		output := process.Output()
		if output == nil {
			readErr = errors.New("backend returned no output stream")
			return
		}
		defer output.Close()
		buffer := make([]byte, 32*1024)
		emptyReads := 0
		for {
			n, err := output.Read(buffer)
			if n < 0 || n > len(buffer) {
				readErr = errors.New("backend returned an invalid read count")
				return
			}
			if n == 0 && err == nil {
				emptyReads++
			} else {
				emptyReads = 0
			}
			if emptyReads >= 100 {
				readErr = io.ErrNoProgress
				return
			}
			p.mu.Lock()
			if n > 0 {
				p.pending.append(buffer[:n])
			}
			p.mu.Unlock()
			p.signal()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readErr = err
				}
				return
			}
		}
	}()
	go func() {
		defer s.workers.Done()
		code, err := waitProcess(process)
		p.mu.Lock()
		p.exited, p.exitCode, p.waitErr = true, code, err
		p.mu.Unlock()
		p.signal()
	}()
	if closed {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := terminateProcess(cleanup, process)
		return nil, &astral.ToolError{Outcome: astral.OutcomeUnknown, Code: "closed_during_start", Cause: errors.Join(errors.New("tool set closed during process startup"), err)}
	}
	return p, nil
}

func waitProcess(process Process) (code int, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("process wait panic: %v", recovered)
		}
	}()
	return process.Wait()
}

func terminateProcess(ctx context.Context, process Process) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("process termination panic: %v", recovered)
		}
	}()
	return process.Terminate(ctx)
}

func (s *Set) collect(ctx context.Context, p *terminal, wait time.Duration, tokens int, start time.Time, emit func(astral.ToolProgress)) astral.ToolResult {
	buffer := headTailBuffer{limit: s.options.MaxBufferedBytes}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	stopped := false
	var complete bool
	var code int
	var waitErr, readErr error
	for {
		p.mu.Lock()
		chunk := p.pending.take()
		complete, code, waitErr, readErr = p.exited && p.outputClosed, p.exitCode, p.waitErr, p.readErr
		p.mu.Unlock()
		if chunk.total > 0 {
			buffer.merge(chunk)
			text, _ := chunk.text()
			emit(astral.ToolProgress{Text: text})
		}
		if complete || stopped || waitErr != nil {
			break
		}
		select {
		case <-ctx.Done():
			stopped = true
		case <-timer.C:
			stopped = true
		case <-p.notify:
		}
	}
	text, _ := buffer.text()
	text = limitCommandOutput(text, buffer.total, tokens)
	result := CommandResult{ChunkID: chunkID(), WallTimeSeconds: time.Since(start).Seconds(), OriginalTokenCount: (buffer.total + 3) / 4, Output: text}
	outcome := astral.OutcomeSucceeded
	problem := ""
	if complete || waitErr != nil {
		if waitErr == nil {
			result.ExitCode = &code
		} else {
			outcome, problem = astral.OutcomeUnknown, "Process wait failed: "+waitErr.Error()
		}
		if readErr != nil {
			problem += "\nOutput collection failed: " + readErr.Error()
		}
		s.mu.Lock()
		if waitErr != nil {
			p.unavailable = true
		} else if s.processes[p.id] == p {
			delete(s.processes, p.id)
		}
		s.mu.Unlock()
	} else {
		result.SessionID = p.id
		s.mu.Lock()
		p.lastUsed = time.Now()
		s.mu.Unlock()
	}
	return commandResult(result, outcome, problem)
}
