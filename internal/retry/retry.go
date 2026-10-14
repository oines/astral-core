// Package retry contains the one retry/backoff loop shared by generation and
// session summarization. It has no knowledge of providers or durable state.
package retry

import (
	"context"
	"errors"
	"time"
)

type Policy struct {
	Attempts int
	Initial  time.Duration
	Maximum  time.Duration
}
type Decision struct {
	Retry bool
	After time.Duration
}

func Do[T any](ctx context.Context, p Policy, attempt func(int) (T, Decision, error), beforeWait func(error)) (T, error) {
	var zero T
	for i := 0; i < p.Attempts; i++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		value, decision, err := attempt(i)
		if err == nil {
			return value, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if !decision.Retry || i+1 == p.Attempts {
			return zero, err
		}
		delay := p.Initial
		for n := 0; n < i && delay < p.Maximum; n++ {
			if delay > p.Maximum/2 {
				delay = p.Maximum
			} else {
				delay *= 2
			}
		}
		if decision.After > delay {
			delay = decision.After
		}
		if beforeWait != nil {
			beforeWait(err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	return zero, errors.New("retry requires a positive attempt limit")
}
