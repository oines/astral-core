package session

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound                = errors.New("astral: session or entry not found")
	ErrConflict                = errors.New("astral: store version conflict")
	ErrCorrupt                 = errors.New("astral: invalid session state")
	ErrBudget                  = errors.New("astral: request exceeds model context budget")
	ErrCompactionInputTooLarge = errors.New("astral: compaction input cannot fit")
	ErrCompactionNoProgress    = errors.New("astral: compaction cannot make progress")
	ErrAlreadyResolved         = errors.New("astral: decision or tool already resolved")
	ErrUnsupportedFormat       = errors.New("astral: unsupported session storage format")
)

// BudgetError reports the estimate for the exact request that was rejected.
type BudgetError struct{ Input, Output, Margin, Window int }

func (e *BudgetError) Error() string {
	return fmt.Sprintf("%v: %d input + %d output + %d margin > %d", ErrBudget, e.Input, e.Output, e.Margin, e.Window)
}
func (e *BudgetError) Unwrap() error { return ErrBudget }
