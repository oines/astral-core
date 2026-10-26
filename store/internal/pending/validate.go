// Package pending validates queue identities within an atomic commit without
// loading the backlog. Reducer owns counts; adapters own the pending index.
package pending

import s "github.com/oines/astral-core"

func Validate(entries []s.Entry, lookup func(string) (s.InputKind, bool, error)) error {
	queued := map[string]bool{}
	added := map[string]s.InputKind{}
	consumed := map[string]bool{}
	cleared := map[s.InputKind]bool{}
	removed := map[string]bool{}
	for _, e := range entries {
		switch e.Kind {
		case s.EntryInputRequeued:
			if !removed[e.Input.ID] {
				return s.ErrCorrupt
			}
			delete(removed, e.Input.ID)
			added[e.Input.ID] = e.Input.Kind
		case s.EntryInputQueued:
			_, exists, err := lookup(e.Input.ID)
			if err != nil {
				return err
			}
			if exists || queued[e.Input.ID] {
				return s.ErrCorrupt
			}
			queued[e.Input.ID] = true
			added[e.Input.ID] = e.Input.Kind
		case s.EntryInputConsumed, s.EntryInputCleared:
			if e.Input == nil {
				for _, kind := range e.Clear.Kinds {
					cleared[kind] = true
					for id, k := range added {
						if k == kind {
							delete(added, id)
						}
					}
				}
				continue
			}
			if consumed[e.Input.ID] {
				return s.ErrCorrupt
			}
			kind, exists := added[e.Input.ID]
			if !exists {
				var err error
				kind, exists, err = lookup(e.Input.ID)
				if err != nil {
					return err
				}
				if cleared[kind] {
					exists = false
				}
			}
			if !exists || removed[e.Input.ID] || kind != e.Input.Kind {
				return s.ErrCorrupt
			}
			consumed[e.Input.ID] = true
			delete(added, e.Input.ID)
			if e.Kind == s.EntryInputCleared {
				removed[e.Input.ID] = true
				delete(consumed, e.Input.ID)
			}
		}
	}
	return nil
}
