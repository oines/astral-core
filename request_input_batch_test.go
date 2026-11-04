package astral

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Preserve the 0139c33 implementation as an independent semantic and cost
// oracle. Both algorithms receive the same frozen recipe in the same binary.
func legacyApplyRequestChange(raw json.RawMessage, path []string, change RequestChange) (json.RawMessage, error) {
	invalid := fmt.Errorf("%w: invalid frozen request change", ErrCorrupt)
	if len(path) == 0 {
		if change.Op != "set" || !json.Valid(change.Value) {
			return nil, invalid
		}
		return change.Value, nil
	}
	if len(raw) == 0 {
		return nil, invalid
	}
	key := path[0]
	if raw[0] == '{' {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return nil, invalid
		}
		if len(path) == 1 && change.Op == "remove" {
			if _, exists := object[key]; !exists {
				return nil, invalid
			}
			delete(object, key)
		} else {
			value, err := legacyApplyRequestChange(object[key], path[1:], change)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		return json.Marshal(object)
	}
	if raw[0] == '[' {
		var array []json.RawMessage
		index, err := strconv.Atoi(key)
		if err != nil || json.Unmarshal(raw, &array) != nil || index < 0 || index > len(array) {
			return nil, invalid
		}
		if len(path) == 1 && change.Op == "insert" {
			if !json.Valid(change.Value) {
				return nil, invalid
			}
			array = append(array, nil)
			copy(array[index+1:], array[index:])
			array[index] = change.Value
		} else if index == len(array) {
			return nil, invalid
		} else if len(path) == 1 && change.Op == "remove" {
			array = append(array[:index], array[index+1:]...)
		} else {
			value, err := legacyApplyRequestChange(array[index], path[1:], change)
			if err != nil {
				return nil, err
			}
			array[index] = value
		}
		return json.Marshal(array)
	}
	return nil, invalid
}

func legacyApplyRequestChanges(raw json.RawMessage, changes []RequestChange) (json.RawMessage, error) {
	var err error
	for _, change := range changes {
		raw, err = legacyApplyRequestChange(raw, change.Path, change)
		if err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func legacyRequestInputApply(input RequestInput, base Request) (Request, error) {
	if input.Selection != nil {
		for _, index := range *input.Selection {
			if index < -1 || index >= len(base.Messages) {
				return Request{}, fmt.Errorf("%w: invalid request message selection", ErrCorrupt)
			}
		}
		base.Messages = selectedMessages(base.Messages, *input.Selection)
	}
	raw, err := json.Marshal(base)
	if err != nil {
		return Request{}, err
	}
	raw, err = legacyApplyRequestChanges(raw, input.Changes)
	if err != nil {
		return Request{}, err
	}
	var request Request
	if err = json.Unmarshal(raw, &request); err != nil {
		return Request{}, err
	}
	if request.ModelID != input.Model.ID || requestDigest(request) != input.Digest {
		return Request{}, fmt.Errorf("%w: frozen request digest mismatch", ErrCorrupt)
	}
	return request, nil
}

func assertRequestChangeBatchMatchesLegacy(t *testing.T, before json.RawMessage, changes []RequestChange) {
	t.Helper()
	legacy, oldErr := legacyApplyRequestChanges(before, changes)
	actual, newErr := applyRequestChanges(before, changes)
	if (oldErr == nil) != (newErr == nil) || oldErr != nil && (oldErr.Error() != newErr.Error() || errors.Is(oldErr, ErrCorrupt) != errors.Is(newErr, ErrCorrupt)) {
		t.Fatalf("change error differs: old=%v new=%v before=%s changes=%+v", oldErr, newErr, before, changes)
	}
	if oldErr == nil && !bytes.Equal(legacy, actual) {
		t.Fatalf("JSON differs: old=%s new=%s changes=%+v", legacy, actual, changes)
	}
}

func TestRequestChangeBatchRetainsSequentialJSONSemantics(t *testing.T) {
	for _, test := range []struct {
		name, before string
		changes      []RequestChange
	}{
		{"opaque numbers duplicates and HTML", `{"opaque":{"n":9007199254740993,"n":9007199254740995,"exponent":1.2300e+99,"html":"<>&\u2028"},"edit":{"x":1,"x":2},"array":[0,1,2]}`, []RequestChange{
			{Path: []string{"edit", "x"}, Op: "set", Value: json.RawMessage(`9007199254740997`)},
			{Path: []string{"array", "+1"}, Op: "insert", Value: json.RawMessage(` {"n":-0.00e-20,"n":9007199254740999} `)},
			{Path: []string{"array", "01", "z"}, Op: "set", Value: json.RawMessage(`"<&>"`)},
			{Path: []string{"array", "-0"}, Op: "remove"},
			{Path: []string{"unknown"}, Op: "set", Value: json.RawMessage(`{"a":1,"a":2}`)},
		}},
		{"replace then descend", `{"a":1}`, []RequestChange{
			{Path: []string{"a"}, Op: "set", Value: json.RawMessage(` { "b": [1,2,3] } `)},
			{Path: []string{"a", "b", "1"}, Op: "remove"},
			{Path: []string{"a", "b", "2"}, Op: "insert", Value: json.RawMessage(`4`)},
			{Path: []string{"a", "b", "0"}, Op: "set", Value: json.RawMessage(`null`)},
		}},
		{"root replacement", `{"before":true}`, []RequestChange{
			{Op: "set", Value: json.RawMessage(`{"items":[{"x":1},2],"opaque":{"x":1,"x":2}}`)},
			{Path: []string{"items", "0", "x"}, Op: "set", Value: json.RawMessage(`3`)},
			{Path: []string{"items", "1"}, Op: "remove"},
		}},
		{"root whitespace remains significant to paths", `null`, []RequestChange{
			{Op: "set", Value: json.RawMessage(` {"x":1} `)},
			{Path: []string{"x"}, Op: "set", Value: json.RawMessage(`2`)},
		}},
		{"empty containers stay distinct", `{"a":[],"o":{}}`, []RequestChange{
			{Path: []string{"a", "0"}, Op: "insert", Value: json.RawMessage(`{}`)},
			{Path: []string{"a", "0"}, Op: "remove"},
			{Path: []string{"a", "0"}, Op: "insert", Value: json.RawMessage(`[]`)},
			{Path: []string{"o", "new"}, Op: "set", Value: json.RawMessage(`1`)},
			{Path: []string{"o", "new"}, Op: "remove"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for end := 1; end <= len(test.changes); end++ {
				assertRequestChangeBatchMatchesLegacy(t, json.RawMessage(test.before), test.changes[:end])
			}
		})
	}
}

func TestRequestChangeBatchRejectsSameDamagedPathsAsLegacy(t *testing.T) {
	values := []json.RawMessage{json.RawMessage(`{"a":[1,{"x":2}],"scalar":1}`), json.RawMessage(`[1,{"x":2}]`), json.RawMessage(`null`), json.RawMessage(` {"a":1}`), json.RawMessage(`{"broken":`)}
	paths := [][]string{nil, {"a"}, {"missing"}, {"a", "0"}, {"a", "2"}, {"a", "3"}, {"a", "-1"}, {"a", "1", "x"}, {"a", "0", "nested"}, {"scalar", "x"}, {"0"}, {"2"}, {"3"}, {"-1"}, {"+1"}, {"01"}, {"999999999999999999999999999999999999"}}
	for _, before := range values {
		for _, path := range paths {
			for _, op := range []string{"set", "insert", "remove", "unknown", ""} {
				for _, value := range []json.RawMessage{nil, json.RawMessage(`1`), json.RawMessage(`null`), json.RawMessage(`{`), json.RawMessage(`1 2`)} {
					assertRequestChangeBatchMatchesLegacy(t, before, []RequestChange{{Path: path, Op: op, Value: value}})
				}
			}
		}
	}
}

func TestRequestChangeBatchRandomizedSequentialEdits(t *testing.T) {
	random := rand.New(rand.NewSource(291))
	for n := 0; n < 300; n++ {
		before := json.RawMessage(`{"a":[{"n":9007199254740993},2,3],"b":{"x":1,"y":2},"untouched":{"dup":1,"dup":2}}`)
		var changes []RequestChange
		for step := 0; step < 8; step++ {
			path := [][]string{{"a", strconv.Itoa(random.Intn(5))}, {"a", strconv.Itoa(random.Intn(4)), "n"}, {"b", []string{"x", "y", "z"}[random.Intn(3)]}}[random.Intn(3)]
			value := []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`{"n":9007199254740995}`), json.RawMessage(`[1,2]`), json.RawMessage(`1e+25`)}[random.Intn(4)]
			changes = append(changes, RequestChange{Path: path, Op: []string{"set", "remove", "insert"}[random.Intn(3)], Value: value})
			assertRequestChangeBatchMatchesLegacy(t, before, changes)
		}
	}
}

func requestBatchImage(t testing.TB) Content {
	t.Helper()
	pixels := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	for at := 0; at < len(pixels.Pix); at += 4 {
		pixels.Pix[at], pixels.Pix[at+1], pixels.Pix[at+2], pixels.Pix[at+3] = byte(at), byte(at>>8), byte(at>>16), 255
	}
	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&encoded, pixels); err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(encoded.Bytes())); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(base64.StdEncoding.EncodeToString(encoded.Bytes()))
	return Content{Type: "image", MediaType: "image/png", Data: data}
}

func requestBatchFixture(t testing.TB) (Request, RequestInput, Request) {
	t.Helper()
	image := requestBatchImage(t)
	base := Context{HeadID: "batch-head", ConfigurationID: "batch-configuration", Request: Request{
		ModelID: "batch-model", ModelRevision: "r1", SystemPrompt: "Frozen synthetic system.", MaxOutputTokens: 4096,
		Messages: []Message{
			User("duplicate context before retained history"),
			{Role: RoleUser, ContextKind: "im.observation", Metadata: json.RawMessage(`{"source":"im","event_id":"event-1","conversation_id":"chat"}`), Content: []Content{{Type: "text", Text: strings.Repeat("Earlier IM image caption. ", 1024)}, image, image, image}},
			{Role: RoleAssistant, Content: []Content{{Type: "reasoning", Data: json.RawMessage(`{"signature":"opaque","integer":9007199254740993}`)}, {Type: "text", Text: "Historical answer."}}, FinishReason: FinishStop},
			User(strings.Repeat("new user input ", 18)),
		},
		Tools:   []ToolDefinition{{Name: "read", Version: "1", Description: "Read a file.", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}},
		Options: GenerationOptions{Reasoning: ReasoningOptions{Effort: "low"}, ProviderOptions: map[string]json.RawMessage{"opaque": json.RawMessage(`{"large":9007199254740993,"nested":{"a":1,"b":2},"unchanged":{"dup":1,"dup":2}}`)}},
	}}
	raw, _ := json.Marshal(base.Request)
	var actual Request
	if err := json.Unmarshal(raw, &actual); err != nil {
		t.Fatal(err)
	}
	actual.Messages = actual.Messages[1:]
	actual.Messages[0].Metadata = json.RawMessage(`{"source":"im","event_id":"event-1","conversation_id":"chat","prepared":true}`)
	actual.Messages[0].Content[0].Text = "Prepared caption with image references retained."
	actual.Messages[1].Content = actual.Messages[1].Content[1:]
	actual.Messages[1].Content[0].Text = "Repaired historical answer."
	actual.Messages[2].Content[0].Text = "Prepared next driving input."
	actual.SystemPrompt += " Fixed prepared environment."
	actual.ModelRevision = "r2"
	actual.MaxOutputTokens = 2048
	actual.Options.Reasoning.Effort = "high"
	actual.Options.ProviderOptions["opaque"] = json.RawMessage(`{"large":9007199254740995,"nested":{"a":3,"c":4},"unchanged":{"dup":1,"dup":2}}`)
	actual.Tools[0].Description = "Read the requested file."
	actual.Tools[0].Parameters = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1}},"required":["path"],"additionalProperties":false}`)
	input, err := FreezeRequest("batch-rollout", base, actual, ModelSpec{ID: actual.ModelID, Revision: actual.ModelRevision, ContextWindow: 128000, MaxOutputTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	return base.Request, input, actual
}

func TestRequestInputBatchApplyMatchesLegacyWireDigestAndOwnership(t *testing.T) {
	base, input, want := requestBatchFixture(t)
	baseBytes, _ := json.Marshal(base)
	changesBefore, _ := json.Marshal(input.Changes)
	old, err := legacyRequestInputApply(input, base)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := input.Apply(base)
	if err != nil {
		t.Fatal(err)
	}
	oldWire, _ := json.Marshal(old)
	wire, _ := json.Marshal(actual)
	if !bytes.Equal(wire, oldWire) || !reflect.DeepEqual(actual, old) || requestDigest(actual) != requestDigest(want) {
		t.Fatal("batch Apply changed final wire, digest or opaque JSON")
	}
	if len(input.Changes) < 10 {
		t.Fatalf("fixture only made %d deltas", len(input.Changes))
	}
	if len(changesBefore) > 16<<10 {
		for _, change := range input.Changes {
			t.Logf("delta path=%v op=%s bytes=%d", change.Path, change.Op, len(change.Value))
		}
		t.Fatalf("recipe copied a referenced image: %d B", len(changesBefore))
	}
	t.Logf("BATCH_FIXTURE base_bytes=%d actual_bytes=%d deltas=%d recipe_bytes=%d digest=%s", len(baseBytes), len(wire), len(input.Changes), len(changesBefore), input.Digest)
	actual.Messages[0].Content[1].Data[0] = 'x'
	actual.Messages[0].Metadata[0] = 'x'
	actual.Tools[0].Parameters[0] = 'x'
	actual.Options.ProviderOptions["opaque"][0] = 'x'
	after, _ := json.Marshal(base)
	changesAfter, _ := json.Marshal(input.Changes)
	if !bytes.Equal(after, baseBytes) || !bytes.Equal(changesAfter, changesBefore) {
		t.Fatal("returned Request aliases its base or frozen changes")
	}
}

func TestRequestInputBatchApplyRetainsSelectionAndCorruptionChecks(t *testing.T) {
	base := Request{ModelID: "model", Messages: []Message{User("a"), User("b")}}
	selection := []int{1, 1}
	want := base
	want.Messages = selectedMessages(base.Messages, selection)
	input := RequestInput{Model: ModelSpec{ID: "model"}, Selection: &selection, Digest: requestDigest(want)}
	actual, err := input.Apply(base)
	if err != nil || !reflect.DeepEqual(actual, want) {
		t.Fatalf("duplicate selection differs: %v", err)
	}
	actual.Messages[0].Content[0].Text = "changed"
	if actual.Messages[1].Content[0].Text != "b" || base.Messages[1].Content[0].Text != "b" {
		t.Fatal("selected messages share mutable content")
	}
	inserted := User("inserted during preparation")
	insertedRaw, _ := json.Marshal(inserted)
	holes := []int{-1, 1, 1}
	withInsertion := input
	withInsertion.Selection = &holes
	withInsertion.Changes = []RequestChange{{Path: []string{"messages", "0"}, Op: "set", Value: insertedRaw}}
	wantInsertion := base
	wantInsertion.Messages = []Message{inserted, base.Messages[1], base.Messages[1]}
	withInsertion.Digest = requestDigest(wantInsertion)
	if rebuilt, err := withInsertion.Apply(base); err != nil || !reflect.DeepEqual(rebuilt, wantInsertion) {
		t.Fatalf("new-message selection placeholder changed: %v", err)
	}
	for _, mutate := range []func(*RequestInput){
		func(i *RequestInput) { v := []int{2}; i.Selection = &v },
		func(i *RequestInput) { v := []int{-2}; i.Selection = &v },
		func(i *RequestInput) { i.Digest = strings.Repeat("0", 64) },
		func(i *RequestInput) { i.Model.ID = "different" },
		func(i *RequestInput) {
			i.Changes = []RequestChange{{Path: []string{"messages", "2"}, Op: "set", Value: json.RawMessage(`{}`)}}
		},
		func(i *RequestInput) {
			i.Changes = []RequestChange{{Path: []string{"options", "missing", "key"}, Op: "set", Value: json.RawMessage(`1`)}}
		},
	} {
		damaged := input
		mutate(&damaged)
		if _, err := damaged.Apply(base); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("damaged frozen input accepted: %v", err)
		}
	}
	root, _ := json.Marshal(want)
	rooted := input
	rooted.Changes = []RequestChange{{Op: "set", Value: root}}
	rootedResult, err := rooted.Apply(base)
	if err != nil || !reflect.DeepEqual(rootedResult, want) {
		t.Fatalf("whole-root replacement differs: %v", err)
	}
	unknown := input
	unknown.Changes = []RequestChange{{Path: []string{"future_field"}, Op: "set", Value: json.RawMessage(`{"n":9007199254740993}`)}}
	if _, err := unknown.Apply(base); err != nil {
		t.Fatalf("unknown field semantics changed: %v", err)
	}
}

func BenchmarkRequestInputApplyBatch(b *testing.B) {
	base, input, _ := requestBatchFixture(b)
	for _, legacy := range []bool{true, false} {
		name := "batch"
		if legacy {
			name = "legacy"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				var err error
				if legacy {
					_, err = legacyRequestInputApply(input, base)
				} else {
					_, err = input.Apply(base)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestIndependentBatchInvalidUTF8PathNormalization(t *testing.T) {
	before := json.RawMessage(`{"\ufffd":1,"b":0}`)
	changes := []RequestChange{
		{Path: []string{string([]byte{0xff})}, Op: "set", Value: json.RawMessage(`2`)},
		{Path: []string{"\ufffd"}, Op: "remove"},
	}
	assertRequestChangeBatchMatchesLegacy(t, before, changes)
}

func TestIndependentApplyInvalidUTF8PathPreservesAcceptedRequest(t *testing.T) {
	base := Request{ModelID: "model", Messages: []Message{User("x")}, Options: GenerationOptions{ProviderOptions: map[string]json.RawMessage{"\ufffd": json.RawMessage(`{"a":1}`)}}}
	want := base
	want.Options.ProviderOptions = nil
	input := RequestInput{Model: ModelSpec{ID: "model"}, Digest: requestDigest(want), Changes: []RequestChange{
		{Path: []string{"options", "provider_options", string([]byte{0xff})}, Op: "set", Value: json.RawMessage(`{"b":2}`)},
		{Path: []string{"options", "provider_options", "\ufffd"}, Op: "remove"},
	}}
	if _, err := legacyRequestInputApply(input, base); err != nil {
		t.Fatalf("old Apply refused fixture: %v", err)
	}
	if _, err := input.Apply(base); err != nil {
		t.Fatalf("new Apply refused formerly accepted request: %v", err)
	}
}
