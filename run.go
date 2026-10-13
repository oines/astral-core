package astral

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type RunResult struct {
	RunID         string
	Status        string
	LastMessageID string
	LastMessage   *Message
}

type Run struct {
	id      string
	session *Session
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	result  RunResult
	err     error
}

func (r *Run) ID() string { return r.id }

func (r *Run) Cancel() { r.cancel() }

// Wait cancellation stops waiting, not the run. Use Cancel to stop execution.
func (r *Run) Wait(ctx context.Context) (RunResult, error) {
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		return clone(r.result), r.err
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	}
}

// Start and Continue use the same driver. Start adds a user message. Continue
// resumes unfinished work or drains queued input without inventing a new prompt.
func (s *Session) Start(ctx context.Context, message Message) (*Run, error) {
	if err := validateUser(message); err != nil {
		return nil, err
	}
	owned := clone(message)
	return s.begin(ctx, &owned)
}

func (s *Session) Continue(ctx context.Context) (*Run, error) { return s.begin(ctx, nil) }

func (s *Session) begin(ctx context.Context, input *Message) (run *Run, err error) {
	parentCtx := ctx
	a := s.agent
	ctx, finish, err := s.beginMaintenance(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	s.mu.Lock()
	path := clone(s.pathLocked())
	mustBeComplete := input != nil || s.boundaryRequiredLocked()
	unresolved := s.unresolvedLocked()
	s.mu.Unlock()
	if len(unresolved) > 0 {
		return nil, &UnknownOutcomeError{ExecutionID: unresolved[0].Invocation.ExecutionID}
	}
	if mustBeComplete {
		if _, err = project(path); err != nil {
			return nil, err
		}
	}
	if input == nil {
		if err = s.recoverInterrupted(ctx); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	needsRefresh := s.needsEnvironment
	previous, _ := effectiveEnvironment(s.pathLocked())
	previous = clone(previous)
	s.mu.Unlock()
	if needsRefresh {
		if a.config.Environment == nil {
			return nil, ErrEnvironmentRefresh
		}
		var snapshot EnvironmentSnapshot
		snapshot, err = a.config.Environment(ctx, EnvironmentRequest{SessionID: s.id, Previous: previous})
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		_, err = s.appendLocked(ctx, Entry{Kind: EntryEnvironment, Environment: &snapshot})
		if err == nil {
			s.needsEnvironment = false
		}
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	configuration := clone(s.configurationLocked())
	s.mu.Unlock()
	if err = a.validateConfiguration(ctx, configuration); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(parentCtx)
	run = &Run{id: newID(), session: s, ctx: runCtx, cancel: cancel, done: make(chan struct{})}
	a.mu.Lock()
	s.mu.Lock()
	if a.closed {
		s.mu.Unlock()
		a.mu.Unlock()
		cancel()
		return nil, ErrClosed
	}
	if err = ctx.Err(); err != nil {
		s.mu.Unlock()
		a.mu.Unlock()
		cancel()
		return nil, err
	}
	entries := []Entry{{Kind: EntryRun, RunID: run.id, Run: &RunRecord{ID: run.id, Status: "started"}}}
	if input != nil {
		entries = append(entries, Entry{Kind: EntryMessage, RunID: run.id, Message: input})
	}
	_, err = s.appendLocked(ctx, entries...)
	if err != nil {
		s.mu.Unlock()
		a.mu.Unlock()
		cancel()
		return nil, err
	}
	s.active = run
	s.endMaintenanceLocked()
	s.publishLocked(Event{Type: EventRunStart, RunID: run.id})
	s.mu.Unlock()
	a.mu.Unlock()
	go s.drive(runCtx, run)
	return run, nil
}

func (s *Session) lastMessageLocked() *Entry {
	path := s.pathLocked()
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].Kind == EntryMessage {
			e := path[i]
			return &e
		}
	}
	return nil
}

func (s *Session) drive(ctx context.Context, run *Run) {
	var runErr error
	defer func() {
		if p := recover(); p != nil {
			runErr = fmt.Errorf("astral: execution panicked: %v", p)
		}
		if runErr != nil {
			s.finishRun(run, runErr)
		}
		run.cancel()
	}()
	s.mu.Lock()
	last := s.lastMessageLocked()
	s.mu.Unlock()
	natural := last == nil
	if last != nil && last.Message.Role == RoleAssistant {
		if len(last.Message.ToolCalls) > 0 {
			natural, runErr = s.executeBatch(ctx, run, *last)
			if runErr != nil {
				return
			}
		} else {
			natural = true
		}
	}
	turns := 0
	for {
		if runErr = ctx.Err(); runErr != nil {
			return
		}
		s.mu.Lock()
		pending := s.pendingInputsLocked(InputSteer)
		if len(pending) == 0 && natural {
			pending = s.pendingInputsLocked(InputFollowUp)
		}
		if len(pending) > 0 {
			item := pending[0]
			_, runErr = s.appendLocked(ctx, Entry{Kind: EntryMessage, RunID: run.id, Message: item.Message}, Entry{Kind: EntryInputConsumed, RunID: run.id, Input: &InputRecord{ID: item.ID, Kind: item.Kind}})
			natural = false
			if runErr != nil {
				s.mu.Unlock()
				return
			}
		} else if natural {
			// Completion and queue observation share the lock. An input either
			// belongs to this run's final checkpoint or stays queued for Continue.
			cleanup, cancel := persistContext(ctx)
			runErr = s.finishLocked(cleanup, run, nil)
			cancel()
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		if s.agent.config.MaxTurns > 0 && turns >= s.agent.config.MaxTurns {
			runErr = ErrTurnLimit
			return
		}
		turns++
		turnID := newID()
		s.publish(Event{Type: EventTurnStart, RunID: run.id, TurnID: turnID})
		var assistant Entry
		assistant, runErr = s.generate(ctx, run, turnID)
		if runErr != nil {
			return
		}
		if len(assistant.Message.ToolCalls) > 0 {
			natural, runErr = s.executeBatch(ctx, run, assistant)
			if runErr != nil {
				return
			}
		} else {
			natural = true
		}
		s.publish(Event{Type: EventTurnEnd, RunID: run.id, TurnID: turnID})
	}
}

func (s *Session) finishRun(run *Run, err error) {
	ctx, cancel := persistContext(context.Background())
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != run {
		return
	}
	_ = s.finishLocked(ctx, run, err)
}

func (s *Session) finishLocked(ctx context.Context, run *Run, runErr error) error {
	status := "completed"
	if runErr != nil {
		status = "failed"
	}
	if runErr != nil && run.ctx.Err() != nil {
		status = "cancelled"
	}
	record := RunRecord{ID: run.id, Status: status}
	if runErr != nil {
		record.Error = runErr.Error()
	}
	_, persistErr := s.appendLocked(ctx, Entry{Kind: EntryRun, RunID: run.id, Run: &record})
	runErr = errors.Join(runErr, persistErr)
	if persistErr != nil {
		status = "failed"
	}
	result := RunResult{RunID: run.id, Status: status}
	if last := s.lastMessageLocked(); last != nil && last.Message.Role == RoleAssistant {
		result.LastMessageID = last.ID
		result.LastMessage = last.Message
	}
	run.mu.Lock()
	run.result = clone(result)
	run.err = runErr
	run.mu.Unlock()
	s.active = nil
	event := Event{Type: EventRunEnd, RunID: run.id}
	if runErr != nil {
		event.Error = runErr.Error()
	}
	s.publishLocked(event)
	close(run.done)
	return runErr
}

func persistContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func isCancelled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
