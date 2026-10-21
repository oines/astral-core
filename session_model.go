package astral

import (
	"context"
)

func (a *Session) model(ctx context.Context, id string) (Model, error) {
	var m Model
	var err error
	if id == a.modelID {
		m = a.config.Model
	} else if a.config.ResolveModel != nil {
		m, err = a.config.ResolveModel(ctx, id)
	} else {
		return nil, fmtInvalid("model %q has no host resolver", id)
	}
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmtInvalid("model resolver returned nil for %q", id)
	}
	spec := m.Spec()
	if err = validateModelSpec(spec); err != nil {
		return nil, err
	}
	if spec.ID != id {
		return nil, fmtInvalid("model resolver returned %q for %q", spec.ID, id)
	}
	return estimatedModel{Model: m, estimate: a.config.EstimateTokens}, nil
}

type estimatedModel struct {
	Model
	estimate TokenEstimator
}

func (m estimatedModel) PrepareRequest(ctx context.Context, request Request) (Request, error) {
	return PrepareRequest(ctx, m.Model, request)
}

func (m estimatedModel) RecoverRequest(ctx context.Context, request Request, cause error) (Request, bool, error) {
	return RecoverModelRequest(ctx, m.Model, request, cause)
}
