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
	EndReason     string
	LastMessageID string
	LastMessage   *Message
}
type Run struct {
	id     string
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	result RunResult
	err    error
}

func (r *Run) ID() string { return r.id }
func (r *Run) Cancel()    { r.cancel() }
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

type PromptResult struct {
	Receipt InputReceipt
	Run     *Run
}

func (s *Session) Prompt(ctx context.Context, input Input) (PromptResult, error) {
	q, err := inputRecord(InputSteer, input)
	if err != nil {
		return PromptResult{}, err
	}
	run, receipt, err := s.begin(ctx, &q)
	return PromptResult{Receipt: receipt, Run: run}, err
}
func (s *Session) Continue(ctx context.Context) (*Run, error) {
	run, _, err := s.begin(ctx, nil)
	return run, err
}
func (s *Session) begin(ctx context.Context, input *InputRecord) (*Run, InputReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, InputReceipt{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, InputReceipt{}, ErrClosed
	}
	if input != nil {
		if receipt, exists, err := s.existingInputLocked(ctx, *input); err != nil || exists {
			s.mu.Unlock()
			return nil, receipt, err
		}
	}
	if s.busyLocked() {
		s.mu.Unlock()
		return nil, InputReceipt{}, ErrBusy
	}
	state := s.executionStateLocked()
	if input == nil && !state.Continuable(state.HasQueuedInputs) {
		s.mu.Unlock()
		return nil, InputReceipt{}, ErrNotContinuable
	}
	if input != nil {
		if !s.state.Branch.Complete() {
			s.mu.Unlock()
			return nil, InputReceipt{}, ErrIncompleteBatch
		}
		if err := s.admitInput(*input); err != nil {
			s.mu.Unlock()
			return nil, InputReceipt{}, err
		}
	}
	runCtx, cancel := context.WithCancel(s.lifetime)
	run := &Run{id: newID(), ctx: runCtx, cancel: cancel, done: make(chan struct{})}
	s.active = run
	s.operationError = ""
	s.attempt = nil
	s.mu.Unlock()
	abandon := func(err error) (*Run, InputReceipt, error) {
		cancel()
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
		close(run.done)
		return nil, InputReceipt{}, err
	}
	if input == nil {
		if err := s.recoverInterrupted(ctx); err != nil {
			return abandon(err)
		}
	}
	s.mu.Lock()
	var drafts []Entry
	if input != nil {
		drafts = append(drafts, Entry{Kind: EntryInputQueued, Input: input})
	}
	drafts = append(drafts, Entry{Kind: EntryRun, RunID: run.id, Run: &RunRecord{ID: run.id, Status: "started", QueueOnly: input == nil && state.LastRunStatus == "cancelled" && state.HasQueuedInputs}})
	entries, err := s.appendLocked(ctx, drafts...)
	s.mu.Unlock()
	if err != nil {
		return abandon(err)
	}
	s.publish(Event{Type: EventRunStart, RunID: run.id})
	go s.drive(runCtx, run)
	receipt := InputReceipt{}
	if input != nil {
		receipt = InputReceipt{InputRecord: *entries[0].Input, State: InputPending, AcceptedAt: entries[0].Time}
	}
	return run, receipt, nil
}
func (s *Session) drive(ctx context.Context, run *Run) {
	var runErr error
	endReason := ""
	endLocked := false
	defer func() {
		if p := recover(); p != nil {
			runErr = fmt.Errorf("astral: execution panicked: %v", p)
		}
		status := "completed"
		if runErr != nil {
			status = "failed"
		}
		if endReason == "" && (ctx.Err() != nil || errors.Is(runErr, context.Canceled)) {
			status = "cancelled"
		}
		cleanup, cancel := persistContext(ctx)
		if !endLocked {
			s.mu.Lock()
		}
		record := RunRecord{ID: run.id, Status: status, EndReason: endReason}
		if runErr != nil {
			record.Error = runErr.Error()
		}
		var err error
		if last := s.state.LastRun; last == nil || last.ID != run.id || last.Status != status || last.EndReason != endReason {
			_, err = s.appendLocked(cleanup, Entry{Kind: EntryRun, RunID: run.id, Run: &record})
		}
		runErr = errors.Join(runErr, err)
		if err != nil {
			status = "failed"
		}
		result := s.runResultLocked(run.id, status)
		result.EndReason = endReason
		var next *Run
		if status == "completed" && endReason != "" && !s.closed && s.lifetime.Err() == nil && (s.state.PendingInputs[InputSteer]+s.state.PendingInputs[InputFollowUp] > 0) {
			nextCtx, nextCancel := context.WithCancel(s.lifetime)
			next = &Run{id: newID(), ctx: nextCtx, cancel: nextCancel, done: make(chan struct{})}
			_, err = s.appendLocked(cleanup, Entry{Kind: EntryRun, RunID: next.id, Run: &RunRecord{ID: next.id, Status: "started"}})
			if err != nil {
				nextCancel()
				next = nil
				runErr = errors.Join(runErr, err)
				s.operationError = err.Error()
			}
		}
		cancel()
		s.active = next
		s.attempt = nil
		s.publishLocked(Event{Type: EventRunEnd, RunID: run.id, Status: status, EndReason: endReason, Error: errorText(runErr)})
		if next != nil {
			s.publishLocked(Event{Type: EventRunStart, RunID: next.id})
		}
		s.mu.Unlock()
		if next != nil {
			go s.drive(next.ctx, next)
		}
		run.mu.Lock()
		run.result, run.err = result, runErr
		run.mu.Unlock()
		run.cancel()
		close(run.done)
	}()
	state := s.executionSnapshot()
	natural := state.QueueOnly || state.Last == nil || state.Last.Message.Role == RoleAssistant
	if state.Last != nil && state.Last.Message.Role == RoleAssistant && state.controlEnd() == nil && len(state.Last.Message.ToolCalls()) > 0 {
		var batch batchOutcome
		batch, runErr = s.executeBatch(ctx, run, *state.Last)
		if runErr != nil {
			return
		}
		natural = batch.Natural
		if batch.EndRun != nil {
			natural = true
			endReason = batch.EndRun.Reason
		}
	}
	for turns := 0; ; turns++ {
		if endReason == "" {
			runErr = ctx.Err()
		}
		if runErr != nil {
			return
		}
		s.mu.Lock()
		queued := s.state.PendingInputs[InputSteer] > 0 || natural && s.state.PendingInputs[InputFollowUp] > 0
		// Natural completion and queue inspection are serialized with admission.
		// If a new input arrives after this boundary, Host's explicit Continue sees
		// the completed lifecycle and can start it without a waiting goroutine.
		if natural && (!queued || endReason != "") {
			passive := endReason == "" && s.state.PendingInputs[InputContext] > 0
			if passive {
				s.mu.Unlock()
				_, _, flushErr := s.prepareStep(ctx, prepareOptions{runID: run.id, passiveOnly: true, reason: "idle", skipCollect: true})
				if flushErr != nil && !errors.Is(flushErr, ErrBudget) {
					runErr = flushErr
					return
				}
				s.mu.Lock()
				queued = s.state.PendingInputs[InputSteer]+s.state.PendingInputs[InputFollowUp] > 0
			}
			if !queued || endReason != "" {
				endLocked = true
				return
			}
		}
		s.mu.Unlock()
		if s.config.MaxTurns > 0 && turns >= s.config.MaxTurns {
			runErr = ErrTurnLimit
			return
		}
		turnID := newID()
		s.publish(Event{Type: EventTurnStart, RunID: run.id, TurnID: turnID})
		var step preparedStep
		step, _, runErr = s.prepareStep(ctx, prepareOptions{runID: run.id, turnID: turnID, natural: natural, reason: "boundary"})
		if runErr == nil {
			var assistant Frame
			assistant, runErr = s.generate(ctx, run, turnID, step)
			if runErr == nil {
				natural = true
				endReason = ""
				if len(assistant.Message.ToolCalls()) > 0 {
					var batch batchOutcome
					batch, runErr = s.executeBatch(ctx, run, assistant)
					natural = batch.Natural
					if batch.EndRun != nil {
						natural = true
						endReason = batch.EndRun.Reason
					}
				}
			}
		}
		s.publish(Event{Type: EventTurnEnd, RunID: run.id, TurnID: turnID, Error: errorText(runErr)})
		if runErr != nil {
			return
		}
	}
}
func (s *Session) lastMessageLocked() *Entry {
	if i, ok := s.windowIndex[s.state.Branch.LastMessageID]; ok {
		e := s.window[i]
		return &e
	}
	return nil
}
func (s *Session) runResultLocked(id, status string) RunResult {
	r := RunResult{RunID: id, Status: status}
	if s.state.LastRun != nil {
		r.EndReason = s.state.LastRun.EndReason
	}
	if last := s.lastMessageLocked(); last != nil && last.Message.Role == RoleAssistant {
		r.LastMessageID, r.LastMessage = last.ID, last.Message
	}
	return clone(r)
}
func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
func persistContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
func isCancelled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
