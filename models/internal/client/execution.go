package client

import (
	"context"
	"encoding/json"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models/internal/utils"
)

// execution owns the final encoded body for exactly one frozen model request.
// Each attempt creates a new reader and current request headers, without
// retaining the caller's mutable Request or protocol projection maps.
type execution struct {
	body   utils.EncodedJSON
	stream func(context.Context, any, func(core.ModelEvent)) (core.Reply, error)
}

func NewExecution(body map[string]any, stream func(context.Context, any, func(core.ModelEvent)) (core.Reply, error)) (core.RequestExecution, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &execution{body: raw, stream: stream}, nil
}

func (e *execution) Stream(ctx context.Context, emit func(core.ModelEvent)) (core.Reply, error) {
	if err := ctx.Err(); err != nil {
		return core.Reply{}, err
	}
	return e.stream(ctx, e.body, emit)
}
