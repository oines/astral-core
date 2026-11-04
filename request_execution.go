package astral

import (
	"context"
	"fmt"
)

// RequestExecution owns a fully prepared wire request. Stream may run again
// for a network retry and must stop emitting before it returns.
type RequestExecution interface {
	Stream(context.Context, func(ModelEvent)) (Reply, error)
}

type RequestExecutionFunc func(context.Context, func(ModelEvent)) (Reply, error)

func (f RequestExecutionFunc) Stream(ctx context.Context, emit func(ModelEvent)) (Reply, error) {
	return f(ctx, emit)
}

// RequestExecutionPreparer optionally freezes encoding after final budget and
// validation. Request data is borrowed read-only and must not be mutated or
// changed after returning. Execution owns the state needed for all retries.
// Preparation has no external effects. A nil result selects legacy Stream,
// which retains its independently mutable request boundary.
type RequestExecutionPreparer interface {
	PrepareExecution(context.Context, Request) (RequestExecution, error)
}

func prepareRequestExecution(ctx context.Context, model Model, request Request) (execution RequestExecution, err error) {
	preparer, ok := model.(RequestExecutionPreparer)
	if !ok {
		return nil, nil
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("model execution preparation panicked: %v", p)
		}
	}()
	return preparer.PrepareExecution(ctx, request)
}

// The final request has passed validation and both budgets. Early usage facts
// already contain its context SHA; overlap the remaining independent digest
// with wire preparation, joining before errors or any commit can escape.
func prepareGenerationExecution(ctx context.Context, model Model, request Request, facts *requestFacts) (RequestExecution, *requestFacts, error) {
	if facts == nil || facts.requestDigest != "" || !facts.matches(request) {
		execution, err := prepareRequestExecution(ctx, model, request)
		return execution, facts, err
	}
	done := make(chan string, 1)
	go func() { done <- requestDigest(request) }()
	execution, err := prepareRequestExecution(ctx, model, request)
	digest := <-done
	if err != nil {
		return execution, facts, err
	}
	// Keep the original early measurement immutable. Its size used the output
	// reservation; bind the new copy to the final, possibly clamped request.
	prepared := *facts
	prepared.size = facts.sizeForOutput(request.MaxOutputTokens)
	prepared.source = request
	prepared.requestDigest = digest // Empty stays a Freeze error, after execution.
	return execution, &prepared, nil
}

type preparedExecutionModel struct {
	Model
	execution RequestExecution
}

func (m preparedExecutionModel) Stream(ctx context.Context, _ Request, emit func(ModelEvent)) (Reply, error) {
	return m.execution.Stream(ctx, emit)
}
