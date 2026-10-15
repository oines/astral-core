package session

import (
	"context"
	"encoding/json"
	"errors"

	protocol "github.com/oines/astral-core"
)

type registeredTool struct {
	tool       protocol.Tool
	definition protocol.ToolDefinition
}
type frozenTool struct{ registeredTool }

func (t frozenTool) Definition() protocol.ToolDefinition { return clone(t.definition) }
func (t frozenTool) Execute(ctx context.Context, i protocol.Invocation, emit func(protocol.ToolProgress)) (protocol.ToolResult, error) {
	return t.tool.Execute(ctx, i, emit)
}
func (t frozenTool) Bind(ctx context.Context, i protocol.Invocation) (json.RawMessage, error) {
	if v, ok := t.tool.(protocol.TargetedTool); ok {
		return v.Bind(ctx, i)
	}
	return i.Binding, nil
}
func (s *Session) initCore() error {
	a := s.agent
	tools := make([]protocol.Tool, 0, len(a.tools))
	for _, d := range a.defaults.Tools {
		tools = append(tools, frozenTool{a.tools[d.Name]})
	}
	c := protocol.Config{ID: s.id, Model: a.config.Model, Tools: tools, ResolveModel: a.config.ResolveModel, Retry: a.config.Retry, Fallback: a.config.Fallback, MaxTurns: a.config.MaxTurns, MaxParallelTools: a.config.MaxParallelTools, AfterToolCall: a.config.AfterToolCall, Driver: sessionDriver{s}}
	if a.config.BeforeToolCall != nil {
		c.BeforeToolCall = func(ctx context.Context, inv protocol.Invocation) (protocol.ToolDecision, error) {
			return a.config.BeforeToolCall(ctx, ToolContext{Invocation: inv, awaitApproval: func(ctx context.Context, r ApprovalRequest) (ApprovalDecision, error) {
				return s.awaitApproval(ctx, inv, r)
			}})
		}
	}
	var err error
	s.core, err = protocol.New(c)
	return err
}

type sessionDriver struct{ s *Session }

func (d sessionDriver) Snapshot() protocol.ExecutionState {
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.configurationLocked()
	env, _ := effectiveEnvironment(s.pathLocked())
	out := protocol.ExecutionState{ID: s.id, Configuration: protocol.Configuration{ModelID: c.ModelID, SystemPrompt: c.SystemPrompt, Tools: c.Tools}, Calls: map[string]protocol.CallState{}}
	if env != nil {
		out.Binding = env.Binding
	}
	if e := s.lastMessageLocked(); e != nil {
		out.Last = &protocol.Frame{ID: e.ID, TurnID: e.TurnID, Message: *e.Message}
		if i, ok := s.windowIndex[s.state.Branch.BatchConfigurationID]; ok && s.window[i].Configuration != nil {
			out.BatchTools = s.window[i].Configuration.Tools
		}
		for _, call := range e.Message.ToolCalls() {
			st := s.toolStateLocked(e.ID, call.ID)
			v := protocol.CallState{}
			if st.prepared != nil {
				v.Invocation = st.prepared.Tool.Invocation
				v.Prepared = true
			}
			if st.authorized != nil {
				v.Decision = st.authorized.Tool.Decision
			}
			if st.started != nil {
				v.Started = true
				v.Invocation = st.started.Tool.Invocation
			}
			if st.completed != nil {
				v.Completed = st.completed.Tool.Result
			}
			if st.result != nil {
				v.Result = st.result.Tool.Result
				v.Invocation = st.result.Tool.Invocation
				v.NeedsResolution = st.result.Tool.NeedsResolution
			}
			out.Calls[call.ID] = v
		}
	}
	return clone(out)
}
func (d sessionDriver) Prepare(ctx context.Context, override string) (protocol.Model, protocol.Request, error) {
	s := d.s
	s.mu.Lock()
	preferred := s.configurationLocked().ModelID
	s.mu.Unlock()
	id := preferred
	if override != "" {
		id = override
	}
	m, err := s.agent.model(ctx, id)
	if err != nil {
		return nil, protocol.Request{}, err
	}
	view, err := s.prepareRequest(ctx, m)
	if err != nil {
		return nil, protocol.Request{}, err
	}
	s.mu.Lock()
	current := s.configurationLocked().ModelID
	s.mu.Unlock()
	if current != preferred {
		m, err = s.agent.model(ctx, current)
		if err == nil {
			view, err = s.requestContext(ctx, m)
		}
	}
	return m, view.Request, err
}
func (d sessionDriver) Commit(ctx context.Context, t protocol.Transition) error {
	s := d.s
	if t.Kind == protocol.TransitionSelectModel {
		return s.SelectModel(ctx, t.ModelID)
	}
	e := Entry{ID: t.ID, RunID: t.RunID, TurnID: t.TurnID}
	var entries []Entry
	switch t.Kind {
	case protocol.TransitionRunStart:
		e.Kind = EntryRun
		e.Run = &RunRecord{ID: t.RunID, Status: "started"}
		entries = append(entries, e)
		if t.Message != nil {
			entries = append(entries, Entry{Kind: EntryMessage, RunID: t.RunID, Message: t.Message})
		}
	case protocol.TransitionMessage:
		e.Kind = EntryMessage
		e.Message = t.Message
		e.Usage = t.Usage
		entries = append(entries, e)
	default:
		kinds := map[protocol.TransitionKind]EntryKind{protocol.TransitionPrepared: EntryToolPrepared, protocol.TransitionAuthorized: EntryToolAuthorized, protocol.TransitionStarted: EntryToolStarted, protocol.TransitionCompleted: EntryToolCompleted, protocol.TransitionResult: EntryToolResult}
		e.Kind = kinds[t.Kind]
		e.Tool = &ToolRecord{Invocation: t.Invocation, Decision: t.Decision, Result: t.Result, NeedsResolution: t.NeedsResolution}
		entries = append(entries, e)
		if t.Kind == protocol.TransitionResult && len(t.Result.Context) > 0 {
			snapshot, err := decodeEnvironment(t.Result.Context)
			if err != nil {
				return err
			}
			entries = append(entries, Entry{Kind: EntryEnvironment, RunID: t.RunID, TurnID: t.TurnID, Environment: snapshot})
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed && t.Kind == protocol.TransitionRunStart {
		return protocol.ErrClosed
	}
	if t.Kind == protocol.TransitionRunStart {
		if !s.maintenance || s.maintenanceContext == nil {
			return protocol.ErrBusy
		}
		// Transfer the reservation to the executor only after its start commit.
		// Cancel and Close can still cancel the preflight/commit context here.
		ctx = s.maintenanceContext
	}
	_, err := s.appendLocked(ctx, entries...)
	if err == nil && t.Kind == protocol.TransitionRunStart {
		s.running = true
		s.endMaintenanceLocked()
	}
	return err
}
func (d sessionDriver) Boundary(ctx context.Context, id string, natural bool) (bool, protocol.RunResult, error) {
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, err := s.pendingInputsLocked(ctx, InputSteer)
	if err != nil {
		return false, protocol.RunResult{}, err
	}
	if len(pending) == 0 && natural {
		pending, err = s.pendingInputsLocked(ctx, InputFollowUp)
		if err != nil {
			return false, protocol.RunResult{}, err
		}
	}
	if len(pending) > 0 {
		item := pending[0]
		_, err := s.appendLocked(ctx, Entry{Kind: EntryMessage, RunID: id, Message: item.Message}, Entry{Kind: EntryInputConsumed, RunID: id, Input: &InputRecord{ID: item.ID, Kind: item.Kind}})
		return false, protocol.RunResult{}, err
	}
	if natural {
		cleanup, cancel := persistContext(ctx)
		defer cancel()
		r, err := s.finishLocked(cleanup, id, "completed", nil)
		return true, r, err
	}
	return false, protocol.RunResult{}, nil
}
func (d sessionDriver) Finish(ctx context.Context, id, status string, cause error) (protocol.RunResult, error) {
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishLocked(ctx, id, status, cause)
}
func (s *Session) finishLocked(ctx context.Context, id, status string, cause error) (protocol.RunResult, error) {
	record := RunRecord{ID: id, Status: status}
	if cause != nil {
		record.Error = cause.Error()
	}
	_, err := s.appendLocked(ctx, Entry{Kind: EntryRun, RunID: id, Run: &record})
	s.running = false
	if err != nil {
		status = "failed"
	}
	r := protocol.RunResult{RunID: id, Status: status}
	if last := s.lastMessageLocked(); last != nil && last.Message.Role == protocol.RoleAssistant {
		r.LastMessageID = last.ID
		r.LastMessage = last.Message
	}
	return clone(r), err
}
func (d sessionDriver) AcceptResult(ctx context.Context, inv protocol.Invocation, r protocol.ToolResult) (protocol.ToolResult, error) {
	return d.s.boundResult(ctx, inv, r)
}
func (s *Session) commitToolResult(ctx context.Context, inv protocol.Invocation, r protocol.ToolResult, needs bool) (protocol.ToolResult, error) {
	cleanup, cancel := persistContext(ctx)
	defer cancel()
	r = normalizeResult(r)
	r, budgetErr := s.boundResult(cleanup, inv, r)
	err := (sessionDriver{s}).Commit(cleanup, protocol.Transition{Kind: protocol.TransitionResult, ID: newID(), RunID: inv.RunID, TurnID: inv.TurnID, Invocation: inv, Result: &r, NeedsResolution: needs})
	if err == nil && needs {
		err = &protocol.UnknownOutcomeError{ExecutionID: inv.ExecutionID}
	}
	return r, errors.Join(budgetErr, err)
}
func (s *Session) Subscribe(buffer int) *protocol.Subscription { return s.core.Subscribe(buffer) }
func (s *Session) publish(e protocol.Event)                    { s.core.Publish(e) }
func (s *Session) publishLocked(e protocol.Event)              { s.core.Publish(e) }
