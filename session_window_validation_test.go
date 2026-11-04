package astral

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type windowValidationStore struct {
	Store
	state RuntimeState
	path  []Entry
}

func (s windowValidationStore) LoadState(context.Context, string) (RuntimeState, error) {
	return clone(s.state), nil
}

func (s windowValidationStore) Scan(context.Context, string, Query) (Page, error) {
	page := Page{}
	for _, entry := range s.path {
		page.Records = append(page.Records, Record{Entry: clone(entry)})
	}
	return page, nil
}

func windowValidationConfig(path []Entry) Config {
	for i := range path {
		path[i].Sequence = uint64(i + 1)
	}
	state := RuntimeState{FormatVersion: FormatVersion, SessionID: "session", RolloutID: "rollout", Version: 1, Branch: Projection{HeadID: path[len(path)-1].ID, ConfigurationID: "configuration"}}
	return Config{Store: windowValidationStore{state: state, path: path}, Model: ModelFunc{Specification: ModelSpec{ID: "model", ContextWindow: 32000, MaxOutputTokens: 1024}}, EstimateTokens: func(context.Context, Model, Request) (int, error) { return 100, nil }}
}

func windowValidationToolPath() []Entry {
	path := compactionBorrowPath()[:2]
	call := protocolCall("call")
	return append(path, Entry{ID: "assistant", Kind: EntryMessage, Message: &call}, Entry{ID: "result", Kind: EntryToolResult, Tool: &ToolRecord{Invocation: Invocation{AssistantID: "assistant", Call: call.ToolCalls()[0]}, Result: &ToolResult{Outcome: OutcomeSucceeded}}})
}

func TestSessionLoadValidatesEffectiveToolResultAndKeepsEmptyContent(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		path := windowValidationToolPath()
		if invalid {
			path[len(path)-1].Tool.Result.Content = []Content{{Type: ""}}
		}
		config := windowValidationConfig(path)
		s, err := OpenSession(context.Background(), config, "session")
		if invalid {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid loaded tool content admitted: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		view, err := s.ReadContext(context.Background())
		if err != nil || len(view.Request.Messages) != 3 || !reflect.DeepEqual(view.Request.Messages[2].Content, Text("")) {
			t.Fatalf("empty historical result no longer projects as empty text: %+v %v", view.Request.Messages, err)
		}
		_ = s.Close(context.Background())
	}
}

func TestSessionLoadAllowsIncompleteBatchButPreparationStillRejectsIt(t *testing.T) {
	path := windowValidationToolPath()
	path = path[:len(path)-1]
	s, err := OpenSession(context.Background(), windowValidationConfig(path), "session")
	if err != nil {
		t.Fatalf("unfinished batch cannot be opened for recovery: %v", err)
	}
	defer s.Close(context.Background())
	if _, _, err := s.prepareStep(context.Background(), prepareOptions{noInputs: true}); !errors.Is(err, ErrIncompleteBatch) {
		t.Fatalf("private preparation accepted unfinished batch: %v", err)
	}
}

func TestSessionWindowValidationPreservesProjectionVisibility(t *testing.T) {
	invalid := func() *Message {
		return &Message{Role: RoleAssistant, Content: []Content{{Type: ""}}, FinishReason: FinishError}
	}
	for _, visibility := range []string{"failed", "compacted", "control", "superseded"} {
		t.Run(visibility, func(t *testing.T) {
			path := compactionBorrowPath()
			switch visibility {
			case "failed":
				path = append(path, Entry{ID: "failed", Kind: EntryMessage, Message: invalid()})
			case "compacted":
				path[1].Message.Content[0].Type = ""
				path = append(path, Entry{ID: "compact", Kind: EntryCompaction, Compaction: &CompactionRecord{ThroughID: "reply", Summary: "summary"}})
			case "control":
				call := protocolCall("control")
				call.Content = append(call.Content, Content{Type: "reasoning", ToolCall: &ToolCall{}})
				path = append(path, Entry{ID: "assistant", Kind: EntryMessage, Message: &call}, Entry{ID: "control", Kind: EntryToolControl, Tool: &ToolRecord{Invocation: Invocation{AssistantID: "assistant", Call: call.ToolCalls()[0]}, Control: &RunEnd{Reason: "done"}}})
			case "superseded":
				path = windowValidationToolPath()
				previous := path[len(path)-1].Tool
				previous.Result.Content = []Content{{Type: ""}}
				previous.NeedsResolution = true
				path = append(path, Entry{ID: "resolution", Kind: EntryToolResolution, Tool: &ToolRecord{Invocation: previous.Invocation, Result: &ToolResult{Content: Text("resolved"), Outcome: OutcomeSucceeded}}})
			}
			want, err := projectBorrowed(path)
			if err != nil {
				t.Fatalf("fixture was not historically projectable: %v", err)
			}
			if err := validateWindowBodies(path); err != nil {
				t.Fatalf("body omitted by projection was newly rejected: %v", err)
			}
			got, err := projectPrepared(path)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("private projection changed: %v", err)
			}
		})
	}
}
