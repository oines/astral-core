package session

import (
	"context"
	"errors"
	"time"

	protocol "github.com/oines/astral-core"
)

func persistContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
func requestError(parent, request context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(request.Err(), context.DeadlineExceeded) {
		return &protocol.ModelError{Code: "request_timeout", Transient: true, Cause: context.DeadlineExceeded}
	}
	return err
}
