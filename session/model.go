package session

import (
	"context"

	protocol "github.com/oines/astral-core"
)

func (a *Manager) model(ctx context.Context, id string) (protocol.Model, error) {
	var m protocol.Model
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
	protocol.Model
	estimate TokenEstimator
}

func (m estimatedModel) PrepareRequest(ctx context.Context, request protocol.Request) (protocol.Request, error) {
	return protocol.PrepareRequest(ctx, m.Model, request)
}
