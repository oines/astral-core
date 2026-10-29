package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
	"github.com/oines/astral-core/models/provider/openai/responses"
	"github.com/oines/astral-core/store/memory"
)

func opaqueUsageReply() core.Reply {
	reply := answer("completed work")
	raw, _ := json.Marshal(map[string]string{"encrypted_content": strings.Repeat("opaque-", 20000)})
	reply.Message.Content = append([]core.Content{{Type: "reasoning", Data: raw}}, reply.Message.Content...)
	reply.Usage = core.Usage{Available: true, InputTokens: 1000, OutputTokens: 75}
	return reply
}

func opaqueContentEstimate(r core.Request) (int, error) {
	n, err := contentEstimate(r)
	for _, message := range r.Messages {
		for _, part := range message.Content {
			n += len(part.Data) / 4
			if part.Type == "image" {
				n += 120
			}
		}
	}
	return n, err
}

func TestCompactionUsesUsageInsideSelectedHistory(t *testing.T) {
	ctx := context.Background()
	m := model(func(_ context.Context, n int, r core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
		if r.SystemPrompt == "summarize" {
			return answer("summary of old work"), nil
		}
		if n == 1 {
			return opaqueUsageReply(), nil
		}
		reply := answer("recent answer retained verbatim")
		reply.Usage = core.Usage{Available: true, InputTokens: 7000, OutputTokens: 100}
		return reply, nil
	})
	m.spec.ContextWindow = 8000
	m.estimator = opaqueContentEstimate
	c := testConfig(m, memory.New())
	c.Compaction = core.CompactionPolicy{Disabled: true, SystemPrompt: "summarize", KeepRecentTokens: 1, OutputTokens: 100}
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	waitOK(t, start(t, s, "old work"))
	waitOK(t, start(t, s, "new question"))
	before := historyOf(s)
	record, err := s.Compact(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if record.InputTokens <= 1075 || record.InputTokens >= 2000 {
		t.Fatalf("used retained reply's usage or re-estimated ciphertext: %d", record.InputTokens)
	}
	request := m.observed()[2]
	raw, _ := json.Marshal(request)
	if !strings.Contains(string(raw), "opaque-") || strings.Contains(requestText(request), "recent answer retained verbatim") {
		t.Fatal("summary changed native input or selected retained history")
	}
	after := historyOf(s)
	if !reflect.DeepEqual(before.Entries, after.Entries[:len(before.Entries)]) {
		t.Fatal("compaction rewrote original history")
	}
}

func TestPassiveCompactionCountsAllUnreportedObservations(t *testing.T) {
	eachStore(t, func(t *testing.T, store core.Store) {
		ctx := context.Background()
		var summary core.Request
		m := model(func(_ context.Context, n int, r core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
			if r.SystemPrompt == "summarize" {
				summary = r
				reply := answer("summary of observed conversation")
				// A summarizer's input is the old history, not the new active window.
				reply.Usage = core.Usage{Available: true, InputTokens: 90000, OutputTokens: 50}
				return reply, nil
			}
			if n != 1 {
				t.Error("passive observations woke the ordinary agent")
			}
			return opaqueUsageReply(), nil
		})
		m.spec.ContextWindow = 4000
		m.estimator = opaqueContentEstimate
		c := testConfig(m, store)
		c.Compaction = core.CompactionPolicy{SystemPrompt: "summarize", KeepRecentTokens: 1, ReserveTokens: 700, OutputTokens: 100}
		s := newSession(t, newSessions(t, c), core.SessionOptions{})
		waitOK(t, start(t, s, "watch this chat"))
		var labels []string
		for i := 0; i < 100 && len(m.observed()) == 1; i++ {
			label := fmt.Sprintf("observation-%03d:", i)
			message := snapshotMessage(label + strings.Repeat("x", 60))
			if i%4 == 0 {
				message.Content = append(message.Content, core.Content{Type: "image", URL: "https://example.invalid/image.png"})
			}
			if _, err := inject(s, ctx, []core.Message{message}); err != nil {
				t.Fatal(err)
			}
			labels = append(labels, label)
		}
		if len(m.observed()) != 2 || len(labels) < 10 || s.State().PendingInputs[core.InputContext] != 0 {
			t.Fatalf("passive pressure did not compact cleanly: calls=%d observations=%d", len(m.observed()), len(labels))
		}
		var record *core.CompactionRecord
		for _, entry := range entries(s) {
			if entry.Compaction != nil {
				record = entry.Compaction
			}
		}
		if record == nil || record.InputTokens < 3000 {
			t.Fatalf("unreported passive tail was not budgeted: %+v", record)
		}
		view, err := s.InspectContext(ctx)
		if err != nil || view.EstimatedInput >= 1000 {
			t.Fatalf("old generation or summary usage leaked into new window: %d %v", view.EstimatedInput, err)
		}
		combined := requestText(summary) + requestText(view.Request)
		for _, label := range labels {
			if strings.Count(combined, label) != 1 {
				t.Fatalf("observation lost or duplicated across compact boundary: %s", label)
			}
		}
		if strings.Contains(requestText(summary), labels[len(labels)-1]) {
			t.Fatal("pending input was summarized before commitment")
		}
		if _, err = inject(s, ctx, []core.Message{snapshotMessage("after compact")}); err != nil {
			t.Fatal(err)
		}
		if len(m.observed()) != 2 {
			t.Fatal("old usage triggered another model request after compact")
		}
	})
}

func TestCompactionUsageCountsResultsAfterToolCallAnchor(t *testing.T) {
	ctx := context.Background()
	result := strings.Repeat("tool-output-", 100)
	m := model(func(_ context.Context, n int, r core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
		if r.SystemPrompt == "summarize" {
			if !strings.Contains(requestText(r), result) || len(r.Messages[1].ToolCalls()) != 1 {
				t.Error("fitting native tool batch was lost or shortened")
			}
			return answer("summary of tool execution"), nil
		}
		if n == 1 {
			reply := opaqueUsageReply()
			reply.Message.Content = append(reply.Message.Content, calls(call("c", "echo", `{"value":"`+result+`"}`)).Message.Content...)
			reply.Message.FinishReason = core.FinishToolCalls
			return reply, nil
		}
		return answer("retained recent answer without usage"), nil
	})
	// The single-result admission check must also fit this synthetic native
	// payload; this test isolates the later summary's tool-result accounting.
	m.spec.ContextWindow = 60000
	m.estimator = opaqueContentEstimate
	c := testConfig(m, memory.New())
	c.Tools = []core.Tool{echoTool(nil)}
	c.Compaction = core.CompactionPolicy{Disabled: true, SystemPrompt: "summarize", KeepRecentTokens: 1, OutputTokens: 100}
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	waitOK(t, start(t, s, "run a tool"))
	record, err := s.Compact(ctx, "")
	if err != nil || record.InputTokens < 1075+len(result) || record.InputTokens > 4000 {
		t.Fatalf("tool result after usage anchor was not counted: %+v %v", record, err)
	}
}

func TestCompactionUsageInvalidatesForChangedRequest(t *testing.T) {
	for _, change := range []string{"model", "snapshot", "revision", "configuration", "prepared_system", "prepared_prefix", "missing_usage"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			changed := false
			base := model(func(_ context.Context, _ int, r core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
				if r.SystemPrompt == "summarize" {
					return answer("summary"), nil
				}
				reply := opaqueUsageReply()
				if change == "missing_usage" {
					reply.Usage.InputMissing = true
				}
				return reply, nil
			})
			base.estimator = opaqueContentEstimate
			m := preparedModel{testModel: base, prepare: func(_ context.Context, r core.Request) (core.Request, error) {
				if changed && change == "prepared_system" && r.SystemPrompt != "summarize" {
					r.SystemPrompt += "new persona"
				}
				if changed && change == "prepared_prefix" && r.SystemPrompt == "summarize" {
					r.Messages[0].Content[0].Text += "transformed source"
				}
				return r, nil
			}}
			c := testConfig(m, memory.New())
			c.Compaction = core.CompactionPolicy{Disabled: true, SystemPrompt: "summarize", KeepRecentTokens: 1, OutputTokens: 100}
			if change == "model" {
				other := model(base.generate)
				other.spec.ID = "other-summary-model"
				other.estimator = opaqueContentEstimate
				c.Compaction.ModelID = other.spec.ID
				c.ResolveModel = func(context.Context, core.ModelSelection) (core.ResolvedModel, error) {
					return core.ResolvedModel{Model: other}, nil
				}
			}
			if change == "snapshot" {
				c.Compaction.ModelID = "summary-alias"
				c.ResolveModel = func(context.Context, core.ModelSelection) (core.ResolvedModel, error) {
					return core.ResolvedModel{Model: m, Snapshot: json.RawMessage(`{"route":"different"}`)}, nil
				}
			}
			s := newSession(t, newSessions(t, c), core.SessionOptions{})
			waitOK(t, start(t, s, "work"))
			if _, err := inject(s, ctx, []core.Message{snapshotMessage("retained latest observation")}); err != nil {
				t.Fatal(err)
			}
			changed = true
			if change == "revision" {
				base.spec.Revision = "new"
			}
			if change == "configuration" {
				configuration := s.Configuration()
				configuration.SystemPrompt = "new configuration"
				if err := s.Configure(ctx, configuration); err != nil {
					t.Fatal(err)
				}
			}
			record, err := s.Compact(ctx, "")
			if err != nil || record.InputTokens < 30000 {
				t.Fatalf("stale usage was reused instead of full estimate: %+v %v", record, err)
			}
		})
	}
}

func TestCompactionUsageWithNativeResponsesReasoning(t *testing.T) {
	ctx := context.Background()
	ciphertext := strings.Repeat("opaque-", 20000)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Instructions string
			Input        json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		var output []any
		if request.Instructions == "summarize" {
			if !strings.Contains(string(request.Input), ciphertext) || !strings.Contains(string(request.Input), "input_image") {
				t.Error("summary dropped native reasoning or image")
			}
		} else {
			output = append(output, map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}, "encrypted_content": ciphertext})
		}
		output = append(output, map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "completed work", "annotations": []any{}}}})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_1", "status": "completed", "model": "fixture", "output": output, "usage": map[string]int{"input_tokens": 1000, "output_tokens": 75}})
	}))
	defer server.Close()
	streaming := false
	m, err := responses.New(models.Config{Spec: core.ModelSpec{ID: "profile", Revision: "1", ContextWindow: 8000, MaxOutputTokens: 1000}, Model: "fixture", BaseURL: server.URL, Streaming: &streaming, ReplayScope: "fixture-connection"})
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig(m, memory.New())
	c.Compaction = core.CompactionPolicy{Disabled: true, SystemPrompt: "summarize", KeepRecentTokens: 1, OutputTokens: 100}
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	waitOK(t, start(t, s, "watch this conversation"))
	image := core.Message{Role: core.RoleUser, Content: []core.Content{{Type: "image", URL: "https://example.invalid/image.png"}}}
	if _, err := inject(s, ctx, []core.Message{image, snapshotMessage("retain this latest observation")}); err != nil {
		t.Fatal(err)
	}
	record, err := s.Compact(ctx, "")
	if err != nil || record.InputTokens < 2275 || record.InputTokens > 4000 || calls.Load() != 2 {
		t.Fatalf("native Responses compaction budget failed: %+v calls=%d err=%v", record, calls.Load(), err)
	}
}

func TestCompactionUsageRejectsRealPassiveTailOverflow(t *testing.T) {
	ctx := context.Background()
	m := model(func(_ context.Context, n int, _ core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
		if n != 1 {
			t.Error("known oversized summary was sent to the model")
		}
		return opaqueUsageReply(), nil
	})
	m.spec.ContextWindow = 4000
	m.estimator = opaqueContentEstimate
	c := testConfig(m, memory.New())
	c.Compaction = core.CompactionPolicy{Disabled: true, SystemPrompt: strings.Repeat("summary", 200), KeepRecentTokens: 1, OutputTokens: 100}
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	waitOK(t, start(t, s, "observe"))
	for i := 0; i < 3; i++ {
		if _, err := inject(s, ctx, []core.Message{snapshotMessage(strings.Repeat("x", 800))}); err != nil {
			t.Fatal(err)
		}
	}
	before := historyOf(s)
	if _, err := s.Compact(ctx, ""); !errors.Is(err, core.ErrCompactionInputTooLarge) {
		t.Fatalf("expected real compaction budget failure: %v", err)
	}
	after := historyOf(s)
	if len(m.observed()) != 1 || !reflect.DeepEqual(before.Entries, after.Entries) {
		t.Fatal("rejected compaction called the model or changed history")
	}
}

func TestCompactionUsageRechecksPendingObservationsBeforeCommit(t *testing.T) {
	ctx := context.Background()
	m := model(func(_ context.Context, _ int, r core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
		if r.SystemPrompt == "summarize" {
			if strings.Contains(requestText(r), "pending-only") {
				t.Error("pending observations entered the old-history summary")
			}
			return answer("summary"), nil
		}
		return opaqueUsageReply(), nil
	})
	m.spec.ContextWindow = 4000
	m.estimator = opaqueContentEstimate
	c := testConfig(m, memory.New())
	c.Compaction = core.CompactionPolicy{Disabled: true, SystemPrompt: "summarize", KeepRecentTokens: 1, OutputTokens: 100}
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	waitOK(t, start(t, s, "work"))
	if _, err := inject(s, ctx, []core.Message{snapshotMessage("retain this observation")}); err != nil {
		t.Fatal(err)
	}
	input := core.Input{ID: "pending", Contexts: []core.Message{snapshotMessage("pending-only" + strings.Repeat("x", 4000))}}
	receipt, err := s.InjectContext(ctx, input)
	if err != nil || receipt.State != core.InputPending {
		t.Fatalf("oversized input was not retained in queue: %+v %v", receipt, err)
	}
	before := historyOf(s)
	if _, err := s.Compact(ctx, ""); !errors.Is(err, core.ErrBudget) {
		t.Fatalf("final candidate did not reject oversized pending input: %v", err)
	}
	if len(m.observed()) != 2 || !reflect.DeepEqual(before.Entries, historyOf(s).Entries) {
		t.Fatal("summary did not run or partial compact was committed")
	}
	receipt, err = s.LookupInput(ctx, input.ID)
	if err != nil || receipt.State != core.InputPending {
		t.Fatalf("failed final budget consumed pending observations: %+v %v", receipt, err)
	}
}

func TestCompactionUsageDefersObservationsArrivingDuringSummary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	m := model(func(ctx context.Context, _ int, r core.Request, _ func(core.ModelEvent)) (core.Reply, error) {
		if r.SystemPrompt == "summarize" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return core.Reply{}, ctx.Err()
			}
			if strings.Contains(requestText(r), "late-observation") {
				t.Error("late input changed the frozen summary")
			}
			return answer("summary"), nil
		}
		return opaqueUsageReply(), nil
	})
	m.spec.ContextWindow = 4000
	m.estimator = opaqueContentEstimate
	c := testConfig(m, memory.New())
	c.Compaction = core.CompactionPolicy{Disabled: true, SystemPrompt: "summarize", KeepRecentTokens: 1, OutputTokens: 100}
	s := newSession(t, newSessions(t, c), core.SessionOptions{})
	waitOK(t, start(t, s, "work"))
	if _, err := inject(s, ctx, []core.Message{snapshotMessage("retained observation")}); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := s.Compact(ctx, "")
		finished <- err
	}()
	signal(t, entered)
	input := core.Input{ID: "late", Contexts: []core.Message{snapshotMessage("late-observation")}}
	receipt, err := s.InjectContext(ctx, input)
	close(release)
	if err != nil || receipt.State != core.InputPending {
		t.Fatalf("late observation was not queued: %+v %v", receipt, err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	view, err := s.InspectContext(ctx)
	if err != nil || strings.Contains(requestText(view.Request), "late-observation") || s.State().PendingInputs[core.InputContext] != 1 {
		t.Fatalf("late observation escaped its input boundary: %v", err)
	}
	receipt, err = s.InjectContext(ctx, input)
	if err != nil || receipt.State != core.InputApplied {
		t.Fatalf("next boundary failed to apply late observation: %+v %v", receipt, err)
	}
	view, err = s.InspectContext(ctx)
	if err != nil || strings.Count(requestText(view.Request), "late-observation") != 1 || len(m.observed()) != 2 {
		t.Fatalf("late observation was lost, duplicated or woke the agent: %v", err)
	}
}
