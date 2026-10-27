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
	return estimatedModel{Model: resolved.Model, estimate: s.config.EstimateTokens, options: clone(resolved.Options), snapshot: append(json.RawMessage(nil), resolved.Snapshot...)}, nil
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
	return estimatedModel{Model: model, estimate: s.config.EstimateTokens, snapshot: append(json.RawMessage(nil), snapshot.Data...)}, nil
}

type estimatedModel struct {
	Model
	estimate TokenEstimator
	options  GenerationOptions
	snapshot json.RawMessage
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
	return PrepareRequest(ctx, m.Model, request)
}

func (m estimatedModel) ValidateRequest(ctx context.Context, request Request) error {
	return ValidateRequest(ctx, m.Model, request)
}

func (m estimatedModel) RecoverRequest(ctx context.Context, request Request, cause error) (Request, bool, error) {
	return RecoverModelRequest(ctx, m.Model, request, cause)
}
