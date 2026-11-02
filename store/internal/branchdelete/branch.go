// Package branchdelete plans backend-independent Rollout branch maintenance.
package branchdelete

import (
	"fmt"
	"sort"

	core "github.com/oines/astral-core"
)

type Node struct {
	ID, ParentID, SessionID string
	History                 bool
}

type Plan struct {
	RolloutID string
	Entire    bool
	Entries   map[string]bool
	Sessions  map[string]bool
}

func Validate(request core.BranchDeleteRequest) error {
	if request.SessionID == "" && request.RolloutID == "" || request.SessionID != "" && (request.RolloutID != "" || request.EntryID != "") {
		return core.ErrInvalid
	}
	return nil
}

// Build sees only one Rollout's metadata, with no conversation bodies loaded.
func Build(request core.BranchDeleteRequest, rollout string, states map[string]core.RuntimeState, nodes []Node) (Plan, error) {
	plan := Plan{RolloutID: rollout, Entries: map[string]bool{}, Sessions: map[string]bool{}}
	if err := Validate(request); err != nil {
		return plan, err
	}
	children := map[string][]string{}
	byID := make(map[string]Node, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
		if node.History {
			children[node.ParentID] = append(children[node.ParentID], node.ID)
		}
	}
	var queue []string
	if request.SessionID != "" {
		state, ok := states[request.SessionID]
		if !ok {
			return plan, nil
		}
		plan.Sessions[request.SessionID] = true
		plan.Entire = state.Creation.Projection == nil || state.Creation.ForkMode != core.ForkShared
		if !plan.Entire {
			for _, node := range nodes {
				if node.History && node.SessionID == request.SessionID {
					queue = append(queue, node.ID)
				}
			}
		}
	} else if request.EntryID == "" {
		plan.Entire = true
	} else {
		node, ok := byID[request.EntryID]
		if !ok {
			return plan, nil
		}
		if !node.History {
			return plan, fmt.Errorf("%w: branch root must be a history node", core.ErrInvalid)
		}
		plan.Entire = node.ParentID == ""
		queue = append(queue, node.ID)
	}
	if plan.Entire {
		for id := range states {
			plan.Sessions[id] = true
		}
		for _, node := range nodes {
			plan.Entries[node.ID] = true
		}
		return plan, nil
	}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if plan.Entries[id] {
			continue
		}
		plan.Entries[id] = true
		queue = append(queue, children[id]...)
	}
	for id, state := range states {
		if plan.Entries[state.Branch.HeadID] || plan.Entries[state.Creation.ForkEntryID] {
			plan.Sessions[id] = true
		}
	}
	// Operational entries have no tree parent and are owned by their Session.
	// Earlier history by the same writer may be shared by surviving branches.
	for _, node := range nodes {
		if !node.History && plan.Sessions[node.SessionID] {
			plan.Entries[node.ID] = true
		}
	}
	return plan, nil
}

func (p Plan) Result() core.BranchDeleteResult {
	result := core.BranchDeleteResult{RolloutID: p.RolloutID, SessionIDs: []string{}}
	for id := range p.Sessions {
		result.SessionIDs = append(result.SessionIDs, id)
	}
	sort.Strings(result.SessionIDs)
	return result
}

func (p Plan) References(ids ...string) error {
	for _, id := range ids {
		if id != "" && p.Entries[id] {
			return fmt.Errorf("%w: surviving data references branch entry %s", core.ErrConflict, id)
		}
	}
	return nil
}

func (p Plan) Projection(value core.Projection) error {
	if err := p.References(value.HeadID, value.ConfigurationID, value.CompactionID, value.ThroughID, value.LastMessageID, value.BatchConfigurationID); err != nil {
		return err
	}
	for _, id := range value.ToolEntries {
		if err := p.References(id); err != nil {
			return err
		}
	}
	for _, tool := range value.Unresolved {
		if err := p.References(tool.Invocation.AssistantID); err != nil {
			return err
		}
	}
	return nil
}

func (p Plan) Entry(entry core.Entry) error {
	if err := p.References(entry.ParentID); err != nil {
		return err
	}
	if entry.Created != nil {
		if err := p.References(entry.Created.ForkEntryID); err != nil {
			return err
		}
		if entry.Created.Projection != nil {
			if err := p.Projection(*entry.Created.Projection); err != nil {
				return err
			}
		}
	}
	if entry.Generation != nil {
		if err := p.References(entry.Generation.ConfigurationID); err != nil {
			return err
		}
	}
	if entry.Compaction != nil {
		if err := p.References(entry.Compaction.ThroughID); err != nil {
			return err
		}
	}
	if entry.Tool != nil {
		return p.References(entry.Tool.Invocation.AssistantID)
	}
	return nil
}

func (p Plan) Record(record core.Record) error {
	if err := p.Entry(record.Entry); err != nil {
		return err
	}
	if record.Projection != nil {
		return p.Projection(*record.Projection)
	}
	return nil
}

func (p Plan) State(state core.RuntimeState) error {
	if err := p.Projection(state.Branch); err != nil {
		return err
	}
	if err := p.References(state.Creation.ForkEntryID); err != nil {
		return err
	}
	if state.Creation.Projection != nil {
		if err := p.Projection(*state.Creation.Projection); err != nil {
			return err
		}
	}
	for _, tool := range state.Tools {
		for _, entry := range []*core.Entry{tool.Prepared, tool.Authorized, tool.Started, tool.Completed, tool.Result, tool.Control, tool.Unresolved} {
			if entry != nil {
				if err := p.References(entry.ID); err != nil {
					return err
				}
				if err := p.Entry(*entry); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p Plan) Attempt(attempt core.ModelAttempt) error {
	if attempt.Input.RolloutID == p.RolloutID {
		if err := p.References(attempt.Input.HeadID, attempt.Input.ConfigurationID); err != nil {
			return err
		}
	}
	if attempt.Outcome != nil {
		return p.References(attempt.Outcome.EntryID)
	}
	return nil
}
