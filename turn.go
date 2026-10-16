package astral

import (
	"context"
	"fmt"
)

type TurnDecision string

const (
	TurnDefault  TurnDecision = ""
	TurnEnd      TurnDecision = "end"
	TurnContinue TurnDecision = "continue"
)

type TurnContext struct {
	SessionID, RunID, TurnID string
	Assistant                Message
	Results                  []ToolResult
}

// FinishTurn must be replay-safe. A confirmed decision is reused on recovery;
// a callback interrupted before its commit may be called again with the TurnID.
type FinishTurn func(context.Context, TurnContext) (TurnDecision, error)

type TurnState struct {
	ID          string       `json:"id"`
	AssistantID string       `json:"assistant_id"`
	Decision    TurnDecision `json:"decision,omitempty"`
	Decided     bool         `json:"decided,omitempty"`
	Advanced    bool         `json:"advanced,omitempty"`
}

func (a *Agent) finishTurn(ctx context.Context, run *Run, assistant Frame) (decision TurnDecision, err error) {
	if assistant.TurnID == "" {
		assistant.TurnID = assistant.ID
	}
	state := a.driver.Snapshot()
	if state.Turn != nil && state.Turn.AssistantID == assistant.ID && state.Turn.Decided {
		return state.Turn.Decision, nil
	}
	turn := TurnContext{SessionID: state.ID, RunID: run.id, TurnID: assistant.TurnID, Assistant: clone(assistant.Message)}
	for _, call := range assistant.Message.ToolCalls() {
		if r := state.Calls[call.ID].Result; r != nil {
			turn.Results = append(turn.Results, clone(*r))
		}
	}
	if a.config.FinishTurn != nil {
		decision, err = func() (d TurnDecision, e error) {
			defer func() {
				if p := recover(); p != nil {
					e = fmt.Errorf("finish-turn hook panicked: %v", p)
				}
			}()
			return a.config.FinishTurn(ctx, turn)
		}()
		if err != nil {
			return "", err
		}
	}
	if decision != TurnDefault && decision != TurnEnd && decision != TurnContinue {
		return "", fmtInvalid("invalid turn decision %q", decision)
	}
	checkpoint := TurnState{ID: assistant.TurnID, AssistantID: assistant.ID, Decision: decision, Decided: true}
	err = a.driver.Commit(ctx, Transition{Kind: TransitionTurnDecision, ID: newID(), RunID: run.id, TurnID: assistant.TurnID, Turn: &checkpoint})
	return decision, err
}
