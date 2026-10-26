package astral

import (
	"context"
	"fmt"
)

func outputReservation(requested int, spec ModelSpec) int {
	if requested == 0 {
		requested = spec.MaxOutputTokens
	}
	if requested > spec.MaxOutputTokens {
		requested = spec.MaxOutputTokens
	}
	return requested
}

func estimate(ctx context.Context, model Model, request Request) (tokens int, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("model estimator panicked: %v", p)
		}
	}()
	estimator, ok := model.(estimatedModel)
	if !ok || estimator.estimate == nil {
		return 0, fmtInvalid("session token estimator is required")
	}
	tokens, err = estimator.estimate(ctx, estimator.Model, clone(request))
	if err == nil && tokens < 0 {
		err = fmtInvalid("model estimator returned a negative count")
	}
	return
}

func checkBudget(ctx context.Context, model Model, request Request, margin int) (int, error) {
	tokens, err := estimate(ctx, model, request)
	if err != nil {
		return 0, err
	}
	return tokens, checkTokenBudget(tokens, model.Spec(), request.MaxOutputTokens, margin)
}

func checkTokenBudget(tokens int, spec ModelSpec, output, margin int) error {
	if spec.MaxInputTokens > 0 && tokens > spec.MaxInputTokens {
		return &BudgetError{Input: tokens, Window: spec.MaxInputTokens}
	}
	// Subtraction avoids integer overflow for hostile or broken estimators.
	if output > spec.ContextWindow || margin > spec.ContextWindow-output || tokens > spec.ContextWindow-output-margin {
		return &BudgetError{Input: tokens, Output: output, Margin: margin, Window: spec.ContextWindow}
	}
	return nil
}
