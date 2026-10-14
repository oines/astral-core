package session

import (
	"context"
	"errors"

	protocol "github.com/oines/astral-core"
)

func (s *Session) prepareRequest(ctx context.Context, model protocol.Model) (Context, error) {
	view, err := s.requestContext(ctx, model)
	near := err == nil && s.agent.config.Budget.AutoCompactAt > 0 && view.EstimatedInput > model.Spec().ContextWindow-view.Request.MaxOutputTokens-s.agent.config.Budget.SafetyMargin-s.agent.config.Budget.AutoCompactAt
	if (errors.Is(err, ErrBudget) || near) && !s.agent.config.Compaction.Disabled {
		_, compactErr := s.compact(ctx, "", model)
		if compactErr != nil {
			if near && errors.Is(compactErr, ErrCompactionNoProgress) {
				return view, nil
			}
			return Context{}, errors.Join(err, compactErr)
		}
		// At most one compaction for this request preparation. This prevents a
		// summary which makes no useful progress from causing an unbounded loop.
		return s.requestContext(ctx, model)
	}
	return view, err
}
