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

func (a *Agent) drive(ctx context.Context, r *Run) {
	var runErr error
	var result RunResult
	finished := false
	defer func() {
		if p := recover(); p != nil {
			runErr = fmt.Errorf("astral: execution panicked: %v", p)
		}
		if !finished {
			status := "failed"
			if ctx.Err() != nil || errors.Is(runErr, context.Canceled) {
				status = "cancelled"
			}
			cleanup, cancel := persistContext(ctx)
			var err error
			result, err = a.driver.Finish(cleanup, r.id, status, runErr)
			cancel()
			runErr = errors.Join(runErr, err)
		}
		r.mu.Lock()
		r.result = clone(result)
		r.err = runErr
		r.mu.Unlock()
		a.mu.Lock()
		e := Event{Type: EventRunEnd, RunID: r.id}
		if runErr != nil {
			e.Error = runErr.Error()
		}
		e.Status = result.Status
		e.EndReason = result.EndReason
		a.Publish(e)
		a.active = nil
		a.mu.Unlock()
		r.cancel()
		close(r.done)
	}()
	state := a.driver.Snapshot()
	natural := state.Last == nil || state.QueueOnly
	if state.Last != nil && state.Last.Message.Role == RoleAssistant {
		natural = true
		if !state.Last.Message.Failed() && state.controlEnd() == nil && (len(state.Last.Message.ToolCalls()) > 0 || state.Turn != nil && !state.Turn.Decided) {
			// Recover a committed assistant/tool boundary without regenerating it.
			recoverTurn := state.Turn == nil || !state.Turn.Decided
			if recoverTurn {
				a.Publish(Event{Type: EventTurnStart, RunID: r.id, TurnID: state.Last.TurnID})
			}
			if len(state.Last.Message.ToolCalls()) > 0 {
				var batch batchOutcome
				batch, runErr = a.executeBatch(ctx, r, *state.Last)
				natural = batch.Natural
				if runErr == nil && batch.EndRun != nil {
					result = controlRunResult(r.id, *state.Last, *batch.EndRun)
					finished = true
				}
			}
			if runErr == nil && recoverTurn && !finished {
				_, runErr = a.finishTurn(ctx, r, *state.Last)
			}
			if recoverTurn {
				event := Event{Type: EventTurnEnd, RunID: r.id, TurnID: state.Last.TurnID}
				if runErr != nil {
					event.Error = runErr.Error()
				}
				a.Publish(event)
			}
			if runErr != nil || finished {
				return
			}
		}
	}
	turns := 0
	for {
		if runErr = ctx.Err(); runErr != nil {
			return
		}
		var done bool
		done, result, runErr = a.driver.Boundary(ctx, r.id, natural)
		if runErr != nil {
			return
		}
		if done {
			finished = true
			return
		}
		if a.config.MaxTurns > 0 && turns >= a.config.MaxTurns {
			runErr = ErrTurnLimit
			return
		}
		turns++
		turnID := newID()
		a.Publish(Event{Type: EventTurnStart, RunID: r.id, TurnID: turnID})
		runErr = func() (err error) {
			defer func() {
				event := Event{Type: EventTurnEnd, RunID: r.id, TurnID: turnID}
				if err != nil {
					event.Error = err.Error()
				}
				a.Publish(event)
			}()
			assistant, err := a.generate(ctx, r, turnID)
			if err != nil {
				return err
			}
			natural = true
			if len(assistant.Message.ToolCalls()) > 0 {
				var batch batchOutcome
				batch, err = a.executeBatch(ctx, r, assistant)
				natural = batch.Natural
				if err != nil {
					return err
				}
				if batch.EndRun != nil {
					result = controlRunResult(r.id, assistant, *batch.EndRun)
					finished = true
					return nil
				}
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			_, err = a.finishTurn(ctx, r, assistant)
			return err
		}()
		if runErr != nil || finished {
			return
		}
	}
}

func controlRunResult(id string, assistant Frame, end RunEnd) RunResult {
	return RunResult{RunID: id, Status: "completed", EndReason: end.Reason, LastMessageID: assistant.ID, LastMessage: &assistant.Message}
}

func persistContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
func isCancelled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
