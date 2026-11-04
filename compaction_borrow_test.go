package astral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func compactionBorrowPath() []Entry {
	user := Message{Role: RoleUser, Content: []Content{{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`)}}}
	reply := Message{Role: RoleAssistant, Content: Text("earlier answer")}
	covered := Request{ModelID: "model", SystemPrompt: "instructions", Messages: []Message{user, reply}}
	return []Entry{
		{ID: "configuration", Kind: EntryConfiguration, Configuration: &Configuration{SystemPrompt: "instructions", Selection: ModelSelection{ID: "model"}}},
		{ID: "user", Kind: EntryMessage, Message: &user},
		{ID: "reply", Kind: EntryMessage, Message: &reply, Usage: Usage{Available: true, InputTokens: 500, OutputTokens: 100}, Generation: &GenerationRecord{ConfigurationID: "configuration", ContextDigest: contextDigest(covered)}},
		{ID: "tail", Kind: EntryMessage, Message: copyPointer(User("retained tail"))},
	}
}

func TestCompactionBorrowedProjectionKeepsReadonlyPayloadIdentity(t *testing.T) {
	path := compactionBorrowPath()
	seenSource, seenSummary := false, false
	model := projectionBoundaryModel{ModelFunc: ModelFunc{Specification: ModelSpec{ID: "model", ContextWindow: 500, MaxOutputTokens: 100}}, project: func(r Request) (Request, error) {
		if r.SystemPrompt == "instructions" || r.SystemPrompt == "summary" {
			if &r.Messages[0].Content[0].Data[0] != &path[1].Message.Content[0].Data[0] {
				t.Fatal("private compaction copied immutable history before its readonly projector")
			}
			seenSource = seenSource || r.SystemPrompt == "instructions"
			seenSummary = seenSummary || r.SystemPrompt == "summary"
		}
		return r, nil
	}}
	wrapped := estimatedModel{Model: model, estimate: func(context.Context, Model, Request) (int, error) { return 1, nil }}
	s := Session{config: Config{Budget: BudgetPolicy{OutputTokens: 100, SafetyMargin: 1}, Compaction: CompactionPolicy{OutputTokens: 100, SystemPrompt: "summary"}}}
	_, err := s.draftCompaction(context.Background(), "", wrapped, path, 1, false, "", "")
	if !errors.Is(err, ErrCompactionInputTooLarge) || !seenSource || !seenSummary {
		t.Fatalf("selected-source usage or final budget changed: source=%v summary=%v err=%v", seenSource, seenSummary, err)
	}
}

func TestCompactionBorrowedHistoryKeepsLegacyCallbackIsolation(t *testing.T) {
	path := compactionBorrowPath()
	want := clone(path)
	calls := 0
	mutate := func(r *Request) {
		for _, message := range r.Messages {
			for _, part := range message.Content {
				if len(part.Data) > 1 {
					part.Data[1] = 'X'
				}
			}
		}
	}
	model := boundaryCopyModel{ModelFunc: ModelFunc{Specification: ModelSpec{ID: "model", ContextWindow: 500, MaxOutputTokens: 100}}, prepare: func(r Request) (Request, error) {
		calls++
		mutate(&r)
		return r, nil
	}}
	wrapped := estimatedModel{Model: model, estimate: func(_ context.Context, _ Model, r Request) (int, error) {
		mutate(&r)
		if r.SystemPrompt == "" {
			return 1, nil
		}
		return 1000, nil
	}}
	s := Session{config: Config{Budget: BudgetPolicy{OutputTokens: 100, SafetyMargin: 1}, Compaction: CompactionPolicy{OutputTokens: 100, SystemPrompt: "summary"}}}
	_, err := s.draftCompaction(context.Background(), "", wrapped, path, 1, false, "", "")
	if !errors.Is(err, ErrCompactionInputTooLarge) || calls == 0 || !reflect.DeepEqual(path, want) {
		t.Fatalf("private borrowed projection weakened legacy callback isolation: calls=%d err=%v", calls, err)
	}
}
