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
func (t frozenTool) Execute(ctx context.Context, i protocol.Invocation, emit func(protocol.ToolProgress)) (protocol.ToolExecution, error) {
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
	c := protocol.Config{ID: s.id, Model: a.config.Model, Tools: tools, ResolveModel: a.config.ResolveModel, Retry: a.config.Retry, Fallback: a.config.Fallback, MaxTurns: a.config.MaxTurns, MaxParallelTools: a.config.MaxParallelTools, MaxSteeringPerTurn: a.config.MaxSteeringPerTurn, AfterToolCall: a.config.AfterToolCall, FinishTurn: a.config.FinishTurn, Driver: sessionDriver{s}}
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
	return s.executionStateLocked()
}
func (s *Session) executionStateLocked() protocol.ExecutionState {
	c := s.configurationLocked()
	out := protocol.ExecutionState{ID: s.id, Configuration: protocol.Configuration{ModelID: c.ModelID, SystemPrompt: c.SystemPrompt, Tools: c.Tools, Options: c.Options}, Calls: map[string]protocol.CallState{}}
	out.Turn = clone(s.state.Turn)
	out.HasQueuedInputs = s.state.PendingInputs[InputSteer]+s.state.PendingInputs[InputFollowUp] > 0
	if s.state.LastRun != nil {
		out.LastRunStatus = s.state.LastRun.Status
		out.LastRunEndReason = s.state.LastRun.EndReason
		out.QueueOnly = s.state.LastRun.QueueOnly
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
			if st.control != nil {
				v.Control = st.control.Tool.Control
				v.Invocation = st.control.Tool.Invocation
			}
			if st.unresolved != nil {
				v.NeedsResolution = true
				v.Invocation = st.unresolved.Tool.Invocation
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed && t.Kind == protocol.TransitionRunStart {
		return protocol.ErrClosed
	}
	e := Entry{ID: t.ID, RunID: t.RunID, TurnID: t.TurnID}
	var entries []Entry
	switch t.Kind {
	case protocol.TransitionRunStart:
		if !s.maintenance || s.maintenanceContext == nil {
			return protocol.ErrBusy
		}
		ctx = s.maintenanceContext
		queued := s.state.PendingInputs[InputSteer]+s.state.PendingInputs[InputFollowUp] > 0
		if t.Message == nil && !s.executionStateLocked().Continuable(queued) {
			return protocol.ErrNotContinuable
		}
		if t.Message != nil {
			if err := s.admitInput(InputRecord{Kind: InputSteer, Message: t.Message, Contexts: t.Contexts}); err != nil {
				return err
			}
			view, err := project(s.pathLocked())
			if err != nil {
				return err
			}
			view.Request.Messages = append(view.Request.Messages, t.Contexts...)
			view.Request.Messages = append(view.Request.Messages, *t.Message)
			raw, err := json.Marshal(view.Request)
			if err != nil {
				return err
			}
			if len(raw) > s.agent.config.MaxContextBytes {
				return ErrBudget
			}
		}
		if s.state.Turn != nil && s.state.Turn.Decided && s.state.Turn.Decision != protocol.TurnContinue && s.state.LastRun != nil && s.state.LastRun.Status != "started" {
			checkpoint := clone(*s.state.Turn)
			checkpoint.Advanced = true
			entries = append(entries, Entry{Kind: EntryTurn, Turn: &checkpoint})
		}
		// Pending passive groups precede a new prompt. The start and its attached
		// batch remain a single commit; already accepted groups stay pending if full.
		if t.Message != nil {
			pending, err := s.pendingRecordsLocked(ctx, InputContext, maxPassivePerBoundary)
			if err != nil {
				return err
			}
			view, err := project(s.pathLocked())
			if err != nil {
				return err
			}
			request := view.Request
			for _, r := range pending {
				q := r.Entry.Input
				trial := request
				trial.Messages = append(append([]protocol.Message(nil), request.Messages...), q.Contexts...)
				fit := trial
				fit.Messages = append(append(append([]protocol.Message(nil), trial.Messages...), t.Contexts...), *t.Message)
				raw, err := json.Marshal(fit)
				if err != nil {
					return err
				}
				if len(raw) > s.agent.config.MaxContextBytes {
					break
				}
				entries = append(entries, contextEntries(q.Contexts, t.RunID, q.ID)...)
				entries = append(entries, Entry{Kind: EntryInputConsumed, Input: &InputRecord{ID: q.ID, Kind: q.Kind}})
				request = trial
			}
		}
		e.Kind = EntryRun
		previous := s.executionStateLocked()
		e.Run = &RunRecord{ID: t.RunID, Status: "started", QueueOnly: t.Message == nil && previous.Last != nil && previous.Last.Message.Role == protocol.RoleUser && previous.LastRunStatus == "cancelled"}
		entries = append(entries, e)
		entries = append(entries, contextEntries(t.Contexts, t.RunID, t.ID)...)
		if t.Message != nil {
			entries = append(entries, Entry{Kind: EntryMessage, RunID: t.RunID, InputID: t.ID, Message: t.Message})
		}
	case protocol.TransitionMessage:
		e.Kind = EntryMessage
		e.Message = t.Message
		e.Usage = t.Usage
		entries = append(entries, e)
		for _, r := range t.SyntheticResults {
			result := r.Result
			entries = append(entries, Entry{ID: r.ID, Kind: EntryToolResult, RunID: t.RunID, TurnID: t.TurnID, Tool: &ToolRecord{Invocation: r.Invocation, Result: &result}})
		}
	case protocol.TransitionTurnStart:
		if s.state.Turn == nil {
			return ctx.Err()
		}
		checkpoint := clone(*s.state.Turn)
		checkpoint.Advanced = true
		e.Kind = EntryTurn
		e.Turn = &checkpoint
		entries = append(entries, e)
	case protocol.TransitionTurnDecision:
		e.Kind = EntryTurn
		e.Turn = t.Turn
		entries = append(entries, e)
	case protocol.TransitionToolControl:
		if t.Control == nil {
			return fmtInvalid("missing tool control")
		}
		var err error
		entries, err = s.controlEntriesLocked(t.ID, t.RunID, t.Invocation, *t.Control)
		if err != nil {
			return err
		}
	default:
		kinds := map[protocol.TransitionKind]EntryKind{protocol.TransitionPrepared: EntryToolPrepared, protocol.TransitionAuthorized: EntryToolAuthorized, protocol.TransitionStarted: EntryToolStarted, protocol.TransitionCompleted: EntryToolCompleted, protocol.TransitionResult: EntryToolResult, protocol.TransitionToolUnresolved: EntryToolUnresolved}
		e.Kind = kinds[t.Kind]
		e.Tool = &ToolRecord{Invocation: t.Invocation, Decision: t.Decision, Result: t.Result, NeedsResolution: t.NeedsResolution}
		entries = append(entries, e)
	}
	_, err := s.appendLocked(ctx, entries...)
	if err == nil && t.Kind == protocol.TransitionRunStart {
		s.running = true
		s.endMaintenanceLocked()
	}
	if err == nil && t.Kind == protocol.TransitionToolControl {
		s.running = false
	}
	return err
}

func (s *Session) mayEndRunLocked(name string) bool {
	i, ok := s.windowIndex[s.state.Branch.BatchConfigurationID]
	if !ok || s.window[i].Configuration == nil {
		return false
	}
	for _, definition := range s.window[i].Configuration.Tools {
		if definition.Name == name {
			return definition.MayEndRun
		}
	}
	return false
}

func (s *Session) controlEntriesLocked(id, runID string, inv protocol.Invocation, end protocol.RunEnd) ([]Entry, error) {
	if !s.mayEndRunLocked(inv.Call.Name) {
		return nil, fmtInvalid("tool %q does not declare MayEndRun", inv.Call.Name)
	}
	return []Entry{
		{ID: id, Kind: EntryToolControl, RunID: runID, TurnID: inv.TurnID, Tool: &ToolRecord{Invocation: inv, Control: &end}},
		{Kind: EntryTurn, RunID: runID, TurnID: inv.TurnID, Turn: &protocol.TurnState{ID: inv.TurnID, AssistantID: inv.AssistantID, Decision: protocol.TurnEnd, Decided: true}},
		{Kind: EntryRun, RunID: runID, Run: &RunRecord{ID: runID, Status: "completed", EndReason: end.Reason}},
	}, nil
}
func (d sessionDriver) Boundary(ctx context.Context, id string, natural bool) (bool, protocol.RunResult, error) {
	s := d.s
	for attempt := 0; attempt < 2; attempt++ {
		s.mu.Lock()
		end := false
		forceContinue := false
		if t := s.state.Turn; t != nil && t.Decided && !t.Advanced {
			end = t.Decision == protocol.TurnEnd
			if t.Decision == protocol.TurnContinue {
				forceContinue = true
			}
		}
		driven, err := s.applyInputsLocked(ctx, id, end, natural)
		if err == nil && (!driven && (natural && !forceContinue || end)) {
			cleanup, cancel := persistContext(ctx)
			r, finishErr := s.finishLocked(cleanup, id, "completed", nil)
			cancel()
			s.mu.Unlock()
			return true, r, finishErr
		}
		modelID := s.configurationLocked().ModelID
		s.mu.Unlock()
		if err == nil {
			return false, protocol.RunResult{}, nil
		}
		if !errors.Is(err, ErrBudget) || attempt > 0 || s.agent.config.Compaction.Disabled {
			return false, protocol.RunResult{}, err
		}
		m, resolveErr := s.agent.model(ctx, modelID)
		if resolveErr != nil {
			return false, protocol.RunResult{}, resolveErr
		}
		if _, compactErr := s.compact(ctx, "", m); compactErr != nil {
			return false, protocol.RunResult{}, errors.Join(err, compactErr)
		}
	}
	return false, protocol.RunResult{}, ErrBudget
}
func (d sessionDriver) Finish(ctx context.Context, id, status string, cause error) (protocol.RunResult, error) {
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if last := s.state.LastRun; last != nil && last.ID == id && last.Status == "completed" {
		return s.runResultLocked(id, last.Status), nil
	}
	_, err := s.applyInputsLocked(ctx, id, true, false)
	r, finishErr := s.finishLocked(ctx, id, status, cause)
	return r, errors.Join(err, finishErr)
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
	return s.runResultLocked(id, status), err
}

func (s *Session) runResultLocked(id, status string) protocol.RunResult {
	r := protocol.RunResult{RunID: id, Status: status}
	if s.state.LastRun != nil {
		r.EndReason = s.state.LastRun.EndReason
	}
	if last := s.lastMessageLocked(); last != nil && last.Message.Role == protocol.RoleAssistant {
		r.LastMessageID = last.ID
		r.LastMessage = last.Message
	}
	return clone(r)
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
