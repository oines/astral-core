package session

import (
	"context"

	protocol "github.com/oines/astral-core"
)

// Start and Continue use the same driver. Start adds a user message. Continue
// resumes unfinished work or drains queued input without inventing a new prompt.
func (s *Session) Start(ctx context.Context, message protocol.Message, options ...protocol.InputOptions) (*protocol.Run, error) {
	if err := validateUser(message); err != nil {
		return nil, err
	}
	owned := clone(message)
	contexts, err := sessionInputOptions(options)
	if err != nil {
		return nil, err
	}
	return s.begin(ctx, &owned, contexts)
}

func (s *Session) Continue(ctx context.Context) (*protocol.Run, error) { return s.begin(ctx, nil, nil) }

func (s *Session) begin(ctx context.Context, input *protocol.Message, contexts []protocol.Message) (run *protocol.Run, err error) {
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
	configuration := clone(s.configurationLocked())
	s.mu.Unlock()
	if err = a.validateConfiguration(ctx, configuration); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input != nil {
		return s.core.Start(parentCtx, *input, protocol.InputOptions{Contexts: contexts})
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
