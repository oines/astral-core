// Package targetlock serializes creators of a stable target within one process.
package targetlock

import (
	"context"
	"sync"
)

type waiter struct {
	token chan struct{}
	users int
}

type Map struct {
	mu      sync.Mutex
	targets map[string]*waiter
}

func (m *Map) Lock(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.targets == nil {
		m.targets = make(map[string]*waiter)
	}
	w := m.targets[id]
	if w == nil {
		w = &waiter{token: make(chan struct{}, 1)}
		m.targets[id] = w
	}
	w.users++
	m.mu.Unlock()
	releaseRef := func() {
		m.mu.Lock()
		w.users--
		if w.users == 0 {
			delete(m.targets, id)
		}
		m.mu.Unlock()
	}
	select {
	case w.token <- struct{}{}:
		return func() { <-w.token; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}
