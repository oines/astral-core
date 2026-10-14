// Package historypage reads bounded ancestor pages for detached history import.
package historypage

import (
	"context"
	"errors"

	s "github.com/oines/astral-core/session"
)

type Reader interface {
	Get(context.Context, string, string) (s.Record, error)
	Scan(context.Context, string, s.Query) (s.Page, error)
}

// Read normally respects DefaultPageBytes. An indivisible record larger than a
// page is copied alone; memory is bounded by the page or largest single record,
// rather than the number of historical entries. Active context is independent.
func Read(ctx context.Context, st Reader, rolloutID, head, cursor string) (s.Page, error) {
	page, err := st.Scan(ctx, rolloutID, s.Query{PathHead: head, Cursor: cursor, Limit: s.DefaultPageSize, MaxBytes: s.DefaultPageBytes})
	if !errors.Is(err, s.ErrBudget) {
		return page, err
	}
	id := cursor
	if id == "" {
		id = head
	}
	record, err := st.Get(ctx, rolloutID, id)
	if err != nil {
		return s.Page{}, err
	}
	if !record.Entry.IsHistory() || record.Projection == nil {
		return s.Page{}, s.ErrCorrupt
	}
	return s.Page{Records: []s.Record{record}, Next: record.Entry.ParentID}, nil
}
