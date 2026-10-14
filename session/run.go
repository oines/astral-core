package session

import (
	"context"

	protocol "github.com/oines/astral-core"
	targetenv "github.com/oines/astral-core/environment"
)

// Start and Continue use the same driver. Start adds a user message. Continue
// resumes unfinished work or drains queued input without inventing a new prompt.
func (s *Session) Start(ctx context.Context, message protocol.Message) (*protocol.Run, error) {
	if err := validateUser(message); err != nil {
		return nil, err
	}
	owned := clone(message)
	return s.begin(ctx, &owned)
}

func (s *Session) Continue(ctx context.Context) (*protocol.Run, error) { return s.begin(ctx, nil) }

func (s *Session) begin(ctx context.Context, input *protocol.Message) (run *protocol.Run, err error) {
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
		return nil, &protocol.UnknownOutcomeError{ExecutionID: unresolved[0].Invocation.ExecutionID}
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
		var snapshot targetenv.Snapshot
		snapshot, err = a.config.Environment(ctx, targetenv.Request{SessionID: s.id, Previous: previous})
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

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input != nil {
		return s.core.Start(parentCtx, *input)
	}
	return s.core.Continue(parentCtx)
}

func (s *Session) lastMessageLocked() *Entry {
	if i, ok := s.windowIndex[s.state.Branch.LastMessageID]; ok {
		e := s.window[i]
		return &e
	}
	return nil
}
