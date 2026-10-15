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
			if ctx.Err() != nil {
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
		a.active = nil
		a.mu.Unlock()
		e := Event{Type: EventRunEnd, RunID: r.id}
		if runErr != nil {
			e.Error = runErr.Error()
		}
		a.Publish(e)
		r.cancel()
		close(r.done)
	}()
	state := a.driver.Snapshot()
	natural := state.Last == nil
	if state.Last != nil && state.Last.Message.Role == RoleAssistant {
		if len(state.Last.Message.ToolCalls()) > 0 {
			natural, runErr = a.executeBatch(ctx, r, *state.Last)
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
		var assistant Frame
		assistant, runErr = a.generate(ctx, r, turnID)
		if runErr != nil {
			return
		}
		if len(assistant.Message.ToolCalls()) > 0 {
			natural, runErr = a.executeBatch(ctx, r, assistant)
			if runErr != nil {
				return
			}
		} else {
			natural = true
		}
		a.Publish(Event{Type: EventTurnEnd, RunID: r.id, TurnID: turnID})
	}
}
func persistContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
func isCancelled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
