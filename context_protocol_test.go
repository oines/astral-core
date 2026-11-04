package astral

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func protocolCall(id string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{Type: "tool_call", ToolCall: &ToolCall{ID: id, Name: "read", Arguments: json.RawMessage(`{}`)}}}}
}

func TestPreparedProjectionMatchesValidatedHistoryAndDrafts(t *testing.T) {
	path := compactionBorrowPath()
	if _, err := projectBorrowed(path); err != nil {
		t.Fatal(err)
	}
	context := Message{Role: RoleUser, ContextKind: "observation", Content: []Content{{Type: "provider", Data: json.RawMessage(`{"payload":"kept"}`)}}}
	if err := context.ValidateContext(); err != nil {
		t.Fatal(err)
	}
	path = append(path, Entry{ID: "context", Kind: EntryContext, Message: &context})
	for _, compact := range []bool{false, true} {
		candidate := append([]Entry(nil), path...)
		if compact {
			candidate = append(candidate, Entry{ID: "compact", Kind: EntryCompaction, Compaction: &CompactionRecord{ThroughID: "reply", Summary: "Earlier task completed."}})
		}
		want, err := projectBorrowed(candidate)
		got, preparedErr := projectPrepared(candidate)
		if err != nil || preparedErr != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("prepared projection changed history/drafts: compact=%v full=%v prepared=%v", compact, err, preparedErr)
		}
		last := got.Request.Messages[len(got.Request.Messages)-1]
		if &last.Content[0].Data[0] != &context.Content[0].Data[0] {
			t.Fatal("prepared projection copied or replaced the validated payload")
		}
	}
}

func TestInitialAndPublicProjectionStillRejectInvalidBodies(t *testing.T) {
	path := compactionBorrowPath()
	path[1].Message.Content[0].Data = json.RawMessage(`{`)
	for _, project := range []func([]Entry) (Context, error){projectBorrowed, project} {
		if _, err := project(path); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unvalidated history passed the full validation boundary: %v", err)
		}
	}
}

func TestPreparedProjectionStillRejectsIncompleteToolTimeline(t *testing.T) {
	path := compactionBorrowPath()
	call := protocolCall("missing")
	if err := call.Validate(); err != nil {
		t.Fatal(err)
	}
	path = append(path, Entry{ID: "call", Kind: EntryMessage, Message: &call})
	_, fullErr := projectBorrowed(path)
	_, preparedErr := projectPrepared(path)
	if !errors.Is(fullErr, ErrIncompleteBatch) || preparedErr == nil || fullErr.Error() != preparedErr.Error() {
		t.Fatalf("prepared projection weakened the tool timeline: full=%v prepared=%v", fullErr, preparedErr)
	}
}

func protocolResult(id string) Message {
	return Message{Role: RoleTool, ToolCallID: id, Content: Text("result")}
}

func TestValidateProtocolPreservesFirstError(t *testing.T) {
	invalid := Message{Role: RoleUser, Content: []Content{{Type: "opaque", Data: json.RawMessage(`{`)}}}
	for _, test := range []struct {
		messages []Message
		want     string
	}{
		{[]Message{protocolResult("orphan"), invalid}, ErrIncompleteBatch.Error() + ": orphan tool result"},
		{[]Message{protocolCall("call"), protocolResult("other"), invalid}, ErrIncompleteBatch.Error() + ": expected result call"},
		{[]Message{protocolCall("call"), {Role: "unknown", Content: Text("invalid")}}, ErrInvalid.Error() + `: unknown message role "unknown"`},
		{[]Message{protocolCall("call"), {Role: RoleTool, Content: Text("invalid")}}, ErrInvalid.Error() + ": tool result without call id"},
	} {
		if err := validateProtocol(test.messages); err == nil || err.Error() != test.want {
			t.Fatalf("first protocol error changed: want=%q got=%v", test.want, err)
		}
	}
}

func TestPreparedProtocolKeepsCompleteBatchOrdering(t *testing.T) {
	for _, test := range []struct {
		name     string
		messages []Message
		valid    bool
	}{
		{"ordinary", []Message{User("hello"), {Role: RoleAssistant, Content: Text("done")}}, true},
		{"complete-batch", []Message{protocolCall("call"), protocolResult("call"), User("next")}, true},
		{"orphan-result", []Message{protocolResult("call")}, false},
		{"missing-result", []Message{protocolCall("call")}, false},
		{"wrong-result", []Message{protocolCall("call"), protocolResult("other")}, false},
		{"interrupted-batch", []Message{protocolCall("call"), User("interrupt"), protocolResult("call")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, message := range test.messages {
				if err := message.Validate(); err != nil {
					t.Fatalf("test needs validated messages: %v", err)
				}
			}
			fullErr := validateProtocol(test.messages)
			preparedErr := validatePreparedProtocol(test.messages)
			if test.valid {
				if fullErr != nil || preparedErr != nil {
					t.Fatalf("valid batch rejected: full=%v prepared=%v", fullErr, preparedErr)
				}
			} else if !errors.Is(preparedErr, ErrIncompleteBatch) || fullErr == nil || preparedErr.Error() != fullErr.Error() {
				t.Fatalf("prepared protocol check changed: full=%v prepared=%v", fullErr, preparedErr)
			}
		})
	}
}
