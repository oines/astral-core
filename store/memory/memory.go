// Package memory supplies an isolated, concurrency-safe, process-local Store.
// Its history indexes intentionally live in memory. Use SQLite for long sessions.
package memory

import (
	"github.com/oines/astral-core/store/internal/ledger"
)

type Store struct{ *ledger.Store }

func New() *Store { return &Store{Store: ledger.New()} }
