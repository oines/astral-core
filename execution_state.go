package astral

// Frame identifies one message independently of any storage representation.
type Frame struct {
	ID      string  `json:"id"`
	TurnID  string  `json:"turn_id,omitempty"`
	Message Message `json:"message"`
}

type CallState struct {
	Invocation      Invocation    `json:"invocation"`
	Prepared        bool          `json:"prepared,omitempty"`
	Decision        *ToolDecision `json:"decision,omitempty"`
	Started         bool          `json:"started,omitempty"`
	Completed       *ToolResult   `json:"completed,omitempty"`
	Result          *ToolResult   `json:"result,omitempty"`
	Control         *RunEnd       `json:"control,omitempty"`
	NeedsResolution bool          `json:"needs_resolution,omitempty"`
}

func (s CallState) Complete() bool { return s.Result != nil || s.Control != nil }

// ExecutionState is only the current execution boundary, not a history tree.
type ExecutionState struct {
	ID               string
	Configuration    Configuration
	BatchTools       []ToolDefinition
	Last             *Frame
	Calls            map[string]CallState
	LastRunStatus    string
	LastRunEndReason string
	// HasQueuedInputs counts driving inputs only; passive context cannot wake a run.
	HasQueuedInputs bool
	// QueueOnly lets queued input lead after cancellation before model generation.
	QueueOnly bool
}

func (s ExecutionState) Continuable(queued bool) bool {
	if queued {
		return true
	}
	if s.Last == nil {
		return false
	}
	if s.Last.Message.Role == RoleUser {
		return true
	}
	if s.Last.Message.Role != RoleAssistant || s.controlEnd() != nil {
		return false
	}
	if s.LastRunStatus == "completed" {
		return false
	}
	calls := s.Last.Message.ToolCalls()
	if len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		state := s.Calls[call.ID]
		if state.NeedsResolution || state.Result == nil || !state.Result.Terminate {
			return true
		}
	}
	return false
}
func (s ExecutionState) controlEnd() *RunEnd {
	for _, call := range s.Calls {
		if call.Control != nil {
			return call.Control
		}
	}
	return nil
}

func (s *Session) executionSnapshot() ExecutionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executionStateLocked()
}
func (s *Session) executionStateLocked() ExecutionState {
	c := s.configurationLocked()
	out := ExecutionState{ID: s.id, Configuration: Configuration{ModelID: c.ModelID, SystemPrompt: c.SystemPrompt, Tools: c.Tools, Options: c.Options}, Calls: map[string]CallState{}}
	out.HasQueuedInputs = s.state.PendingInputs[InputSteer]+s.state.PendingInputs[InputFollowUp] > 0
	if s.state.LastRun != nil {
		out.LastRunStatus = s.state.LastRun.Status
		out.LastRunEndReason = s.state.LastRun.EndReason
		out.QueueOnly = s.state.LastRun.QueueOnly
	}
	if e := s.lastMessageLocked(); e != nil {
		out.Last = &Frame{ID: e.ID, TurnID: e.TurnID, Message: *e.Message}
		if i, ok := s.windowIndex[s.state.Branch.BatchConfigurationID]; ok && s.window[i].Configuration != nil {
			out.BatchTools = s.window[i].Configuration.Tools
		}
		for _, call := range e.Message.ToolCalls() {
			st := s.toolStateLocked(e.ID, call.ID)
			v := CallState{}
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
