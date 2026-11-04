package astral

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMessageValidationPreservesJSONErrorsAndOpaquePayloads(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, {}, json.RawMessage(`null`), json.RawMessage(` {"opaque":900719925474099312345} `), {'"', 0xff, '"'}, json.RawMessage(`{`), json.RawMessage(`{} {}`)} {
		for _, field := range []string{"data", "part-metadata", "call-metadata", "message-metadata"} {
			t.Run(field+"/"+string(raw), func(t *testing.T) {
				message := Message{Role: RoleAssistant, Content: []Content{{Type: "tool_call", ToolCall: &ToolCall{ID: "call", Name: "read", Arguments: json.RawMessage(`{}`)}}}}
				switch field {
				case "data":
					message.Content[0].Data = raw
				case "part-metadata":
					message.Content[0].Metadata = raw
				case "call-metadata":
					message.Content[0].ToolCall.Metadata = raw
				case "message-metadata":
					message.Metadata = raw
				}
				before := append([]byte(nil), raw...)
				_, encodeErr := json.Marshal(message)
				err := message.Validate()
				if encodeErr == nil && err != nil {
					t.Fatal(err)
				}
				if encodeErr != nil {
					want := fmtInvalid("message is not JSON serializable: %v", encodeErr)
					if err == nil || err.Error() != want.Error() {
						t.Fatalf("encoder error changed: want=%v got=%v", want, err)
					}
				}
				if len(raw) > 0 && !reflect.DeepEqual([]byte(raw), before) {
					t.Fatal("validation mutated opaque payload")
				}
			})
		}
	}
}

func TestGenerationProvenancePreservesDigestAndCallerStorage(t *testing.T) {
	request := completeCopyRequest()
	// An unused slot is still caller-owned and must not receive the reply.
	storage := make([]Message, len(request.Messages)+1)
	copy(storage, request.Messages)
	storage[len(request.Messages)] = User("sentinel")
	request.Messages = storage[:len(request.Messages)]
	reply := Message{Role: RoleAssistant, Content: Text("done"), FinishReason: FinishStop}
	covered := clone(request)
	covered.Messages = append(covered.Messages, reply)
	attempt := ModelAttempt{ID: "attempt", Input: RequestInput{ConfigurationID: "configuration", Model: ModelSpec{ID: "model"}}}
	got := GenerationProvenance(attempt, request, reply)
	if got.ContextDigest != contextDigest(covered) || got.AttemptID != attempt.ID || got.ConfigurationID != attempt.Input.ConfigurationID || got.Model != attempt.Input.Model {
		t.Fatalf("generation provenance changed: %+v", got)
	}
	if !reflect.DeepEqual(storage[len(request.Messages)], User("sentinel")) || !reflect.DeepEqual(request.Messages, completeCopyRequest().Messages) {
		t.Fatal("generation provenance changed caller-owned messages")
	}
}
