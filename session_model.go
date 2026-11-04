package astral

import (
	"context"
	"encoding/json"
)

func (s *Session) resolveModel(ctx context.Context, selection ModelSelection) (Model, error) {
	if err := selection.Validate(); err != nil {
		return nil, err
	}
	var resolved ResolvedModel
	var err error
	if selection.ID == s.modelID && s.config.Model != nil {
		resolved = ResolvedModel{Model: s.config.Model, Options: selection.Options}
		if selection.Reasoning != nil {
			resolved.Options.Reasoning = *selection.Reasoning
		}
	} else if s.config.ResolveModel != nil {
		resolved, err = s.config.ResolveModel(ctx, clone(selection))
	} else {
		return nil, fmtInvalid("model %q has no resolver", selection.ID)
	}
	if err != nil {
		return nil, err
	}
	if resolved.Model == nil {
		return nil, fmtInvalid("model resolver returned nil for %q", selection.ID)
	}
	if err = validateModelSpec(resolved.Model.Spec()); err != nil {
		return nil, err
	}
	if err = resolved.Options.Validate(); err != nil {
		return nil, err
	}
	if len(resolved.Snapshot) != 0 && !json.Valid(resolved.Snapshot) {
		return nil, fmtInvalid("model snapshot must be valid JSON")
	}
	estimate, readOnly := s.tokenEstimator()
	return estimatedModel{Model: resolved.Model, estimate: estimate, estimateReadOnly: readOnly, options: clone(resolved.Options), snapshot: append(json.RawMessage(nil), resolved.Snapshot...)}, nil
}

func (s *Session) restoreModel(ctx context.Context, snapshot ModelSnapshot) (Model, error) {
	var model Model
	var err error
	if s.config.RestoreModel != nil {
		model, err = s.config.RestoreModel(ctx, clone(snapshot))
	} else if s.config.Model != nil && s.config.Model.Spec() == snapshot.Spec && len(snapshot.Data) == 0 {
		model = s.config.Model
	} else {
		return nil, fmtInvalid("frozen model %q has no restore callback", snapshot.Spec.ID)
	}
	if err != nil {
		return nil, err
	}
	if model == nil || model.Spec() != snapshot.Spec {
		return nil, fmtInvalid("restored model does not match frozen specification")
	}
	estimate, readOnly := s.tokenEstimator()
	return estimatedModel{Model: model, estimate: estimate, estimateReadOnly: readOnly, snapshot: append(json.RawMessage(nil), snapshot.Data...)}, nil
}

func (s *Session) tokenEstimator() (TokenEstimator, bool) {
	if s.config.EstimateTokensReadOnly != nil {
		return s.config.EstimateTokensReadOnly, true
	}
	return s.config.EstimateTokens, false
}

type estimatedModel struct {
	Model
	estimate         TokenEstimator
	estimateReadOnly bool
	options          GenerationOptions
	snapshot         json.RawMessage
}

func resolvedOptions(model Model) GenerationOptions {
	return clone(model.(estimatedModel).options)
}

func freezeModelRequest(rolloutID string, base Context, request Request, model Model) (RequestInput, error) {
	input, err := FreezeRequest(rolloutID, base, request, model.Spec())
	if err == nil {
		input.Snapshot = append(json.RawMessage(nil), model.(estimatedModel).snapshot...)
	}
	return input, err
}

func (m estimatedModel) PrepareRequest(ctx context.Context, request Request) (Request, error) {
	// The outer Core boundary owns isolation and validation. Re-entering it
	// here would copy the same request again around the underlying hook.
	if preparer, ok := m.Model.(RequestPreparer); ok {
		return preparer.PrepareRequest(ctx, request)
	}
	return request, nil
}

func (m estimatedModel) ProjectRequest(ctx context.Context, request Request) (Request, error) {
	if projector, ok := m.Model.(RequestProjector); ok {
		return projector.ProjectRequest(ctx, request)
	}
	return PrepareRequest(ctx, m.Model, request)
}

func (m estimatedModel) ValidateRequest(ctx context.Context, request Request) error {
	if validator, ok := m.Model.(RequestValidator); ok {
		return validator.ValidateRequest(ctx, request)
	}
	return nil
}

func (m estimatedModel) ValidateRequestReadOnly(ctx context.Context, request Request) error {
	// Dispatch through Core so a legacy underlying validator still receives an
	// isolated copy, while a read-only validator (or no hook) needs no copy.
	return ValidateRequest(ctx, m.Model, request)
}

func (m estimatedModel) PrepareExecution(ctx context.Context, request Request) (RequestExecution, error) {
	if preparer, ok := m.Model.(RequestExecutionPreparer); ok {
		return preparer.PrepareExecution(ctx, request)
	}
	return nil, nil
}

func (m estimatedModel) RecoverRequest(ctx context.Context, request Request, cause error) (Request, bool, error) {
	return RecoverModelRequest(ctx, m.Model, request, cause)
}
