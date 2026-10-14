package session

import (
	"context"
	"fmt"

	protocol "github.com/oines/astral-core"
)

func outputReservation(requested int, spec protocol.ModelSpec) int {
	if requested == 0 {
		requested = 16384
	}
	if requested > spec.MaxOutputTokens {
		requested = spec.MaxOutputTokens
	}
	return requested
}

func estimate(ctx context.Context, model protocol.Model, request protocol.Request) (tokens int, err error) {
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

func checkBudget(ctx context.Context, model protocol.Model, request protocol.Request, margin int) (int, error) {
	tokens, err := estimate(ctx, model, request)
	if err != nil {
		return 0, err
	}
	spec := model.Spec()
	// Subtraction avoids integer overflow for hostile or broken estimators.
	if request.MaxOutputTokens > spec.ContextWindow || margin > spec.ContextWindow-request.MaxOutputTokens || tokens > spec.ContextWindow-request.MaxOutputTokens-margin {
		return tokens, &BudgetError{Input: tokens, Output: request.MaxOutputTokens, Margin: margin, Window: spec.ContextWindow}
	}
	return tokens, nil
}
