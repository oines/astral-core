//go:build ignore

// Generate only with Core 027b81ca29b6df73f9c37c2421452a1e242a6930.
// Copy into that checkout and run with a new output database path.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/store/sqlite"
)

type offline struct{ calls atomic.Uint64 }

type waitingModel struct{ entered chan struct{} }

func (*waitingModel) Spec() core.ModelSpec { return (&offline{}).Spec() }
func (m *waitingModel) Stream(ctx context.Context, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
	close(m.entered)
	<-ctx.Done()
	return core.Reply{}, ctx.Err()
}

func (*offline) Spec() core.ModelSpec {
	return core.ModelSpec{ID: "fixture", ContextWindow: 1000000, MaxOutputTokens: 1024}
}
func (*offline) PrepareRequest(_ context.Context, r core.Request) (core.Request, error) {
	var kept []core.Message
	for _, m := range r.Messages {
		if m.ContextKind == "replace" {
			m.Content = core.Text("unrelated replacement")
			m.Metadata = json.RawMessage(`{"replacement":true}`)
		}
		if m.ContextKind != "omit" {
			kept = append(kept, m)
		}
	}
	r.Messages = kept
	return r, nil
}
func (m *offline) Stream(_ context.Context, r core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
	if len(r.Tools) > 0 && len(r.Messages) > 0 && r.Messages[len(r.Messages)-1].Role == core.RoleUser {
		call := core.ToolCall{ID: fmt.Sprint("call-", m.calls.Add(1)), Name: "echo", Arguments: json.RawMessage(`{}`)}
		return core.Reply{Message: core.Message{Role: core.RoleAssistant, FinishReason: core.FinishToolCalls, Content: []core.Content{{Type: "tool_call", ToolCall: &call}}}}, nil
	}
	return core.Reply{Message: core.Message{Role: core.RoleAssistant, FinishReason: core.FinishStop, Content: core.Text("synthetic fixture summary or answer")}}, nil
}
func main() {
	if err := run(); err != nil {
		panic(err)
	}
}
func run() error {
	if core.FormatVersion != 11 {
		return fmt.Errorf("fixture requires actual Core format 11")
	}
	ctx := context.Background()
	st, err := sqlite.Open(os.Args[1])
	if err != nil {
		return err
	}
	defer st.Close()
	c := core.Config{Store: st, Model: &offline{}, EstimateTokens: func(context.Context, core.Model, core.Request) (int, error) { return 100, nil }, Budget: core.BudgetPolicy{SafetyMargin: 1}, Retry: core.RetryPolicy{MaxAttempts: 1}, Compaction: core.CompactionPolicy{Disabled: true, KeepRecentTokens: 1, OutputTokens: 128}, Tools: []core.Tool{core.ToolFunc{Spec: core.ToolDefinition{Name: "echo", Version: "1", Parameters: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, core.Invocation, func(core.ToolProgress)) (core.ToolExecution, error) {
		return core.ToolExecution{Result: &core.ToolResult{Content: core.Text("synthetic result")}}, nil
	}}}}
	passive := func(s *core.Session, id, kind string) error {
		_, err := s.InjectContext(ctx, core.Input{ID: id, Contexts: []core.Message{{Role: core.RoleUser, ContextKind: kind, Content: core.Text("synthetic " + id)}}})
		return err
	}
	prompt := func(s *core.Session, id string) error {
		p, err := s.Prompt(ctx, core.Input{ID: id, Message: core.User("synthetic work")})
		if err != nil {
			return err
		}
		_, err = p.Run.Wait(ctx)
		return err
	}
	parent, err := core.NewSession(ctx, c, core.SessionOptions{ID: "parent"})
	if err != nil {
		return err
	}
	defer parent.Close(ctx)
	if err = passive(parent, "seen", "observation"); err != nil {
		return err
	}
	if err = prompt(parent, "parent-work"); err != nil {
		return err
	}
	if err = passive(parent, "late", "observation"); err != nil {
		return err
	}
	for i := range 5 {
		if err = passive(parent, fmt.Sprint("unseen-summary-", i), "observation"); err != nil {
			return err
		}
	}
	if _, err = parent.Compact(ctx, ""); err != nil {
		return err
	}
	child, err := parent.Fork(ctx, parent.Head(), core.ForkOptions{ID: "child", Mode: core.ForkShared})
	if err != nil {
		return err
	}
	defer child.Close(ctx)
	if err = prompt(child, "child-work"); err != nil {
		return err
	}
	summary, err := core.NewSession(ctx, c, core.SessionOptions{ID: "summary"})
	if err != nil {
		return err
	}
	defer summary.Close(ctx)
	for i := range 5 {
		if err = passive(summary, fmt.Sprint("seen-summary-", i), "observation"); err != nil {
			return err
		}
	}
	if _, err = summary.Compact(ctx, ""); err != nil {
		return err
	}
	if err = prompt(summary, "summary-work"); err != nil {
		return err
	}
	omit, err := core.NewSession(ctx, c, core.SessionOptions{ID: "omission"})
	if err != nil {
		return err
	}
	defer omit.Close(ctx)
	if err = passive(omit, "omitted", "omit"); err != nil {
		return err
	}
	if err = passive(omit, "included", "observation"); err != nil {
		return err
	}
	if err = prompt(omit, "omission-work"); err != nil {
		return err
	}
	replacement, err := core.NewSession(ctx, c, core.SessionOptions{ID: "replacement"})
	if err != nil {
		return err
	}
	defer replacement.Close(ctx)
	if err = passive(replacement, "replaced", "replace"); err != nil {
		return err
	}
	if err = prompt(replacement, "replacement-work"); err != nil {
		return err
	}
	blocked := &waitingModel{entered: make(chan struct{})}
	stoppedConfig := c
	stoppedConfig.Model = blocked
	stoppedConfig.Tools = nil
	stopped, err := core.NewSession(ctx, stoppedConfig, core.SessionOptions{ID: "stopped"})
	if err != nil {
		return err
	}
	defer stopped.Close(ctx)
	activation, err := stopped.Prompt(ctx, core.Input{ID: "stopped-work", Message: core.User("synthetic stopped work")})
	if err != nil {
		return err
	}
	<-blocked.entered
	if err = passive(stopped, "unseen-stopped", "observation"); err != nil {
		return err
	}
	activation.Run.Cancel()
	if _, err = activation.Run.Wait(ctx); !errors.Is(err, context.Canceled) {
		return fmt.Errorf("expected fixture cancellation: %v", err)
	}
	return nil
}
