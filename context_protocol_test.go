package astral

import (
	"encoding/json"
	"errors"
	"testing"
)

func protocolCall(id string) Message {
	return Message{Role: RoleAssistant, Content: []Content{{Type: "tool_call", ToolCall: &ToolCall{ID: id, Name: "read", Arguments: json.RawMessage(`{}`)}}}}
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
