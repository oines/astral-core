package standardtools

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCanonicalEditPreparationReusesInputWithoutRewriting(t *testing.T) {
	for _, input := range []string{
		` {"path": "f", "edits": [ {"oldText":"a", "newText":"b"} ]} `,
		`{"edits":[],"path":"f","unknown":{"literal":"oldText, newText, edits","array":[1,null,true]}}`,
		`{"path":"f","oldText":123,"newText":"ignored","edits":[{"oldText":"a","newText":"b"}]}`,
		`{"path":"f","OLDTEXT":"ignored","NEWTEXT":"ignored","edits":[]}`,
		`{"path":"f","edits":"ignored first key","edits":[],"unknown":42}`,
		`{"path":"f","oldText":"ignored first key","oldText":null,"newText":"ignored","edits":[]}`,
		`{"path":"f","e\u0064its":[{"oldText":"a","newText":"b"}],"unknown":"unchanged"}`,
	} {
		t.Run(input, func(t *testing.T) {
			raw := json.RawMessage(input)
			before := bytes.Clone(raw)
			got, err := prepareEditArguments(raw)
			if err != nil || !bytes.Equal(raw, before) || !bytes.Equal(got, before) || &got[0] != &raw[0] {
				t.Fatalf("canonical arguments were copied, rewritten or mutated: %s %v", got, err)
			}
		})
	}
}

func TestEditPreparationFastPathPreservesExactKeysAndLegacyPriority(t *testing.T) {
	for _, test := range []struct{ input, expected string }{
		{`{"path":"f","edits":{"oldText":"a","newText":"b"},"EDITS":[]}`, `{"path":"f","edits":[{"oldText":"a","newText":"b"}],"EDITS":[]}`},
		{`{"path":"f","edits":[],"oldText":"a","newText":"b"}`, `{"path":"f","edits":[{"oldText":"a","newText":"b"}]}`},
		{`{"path":"f","edits":[],"oldText":null,"oldText":"a","newText":"b"}`, `{"path":"f","edits":[{"oldText":"a","newText":"b"}]}`},
		{`{"path":"f","edits":[],"edits":{"oldText":"a","newText":"b"}}`, `{"path":"f","edits":[{"oldText":"a","newText":"b"}]}`},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := prepareEditArguments(json.RawMessage(test.input))
			if err != nil || !sameJSON(got, json.RawMessage(test.expected)) {
				t.Fatalf("got %s %v; want %s", got, err, test.expected)
			}
		})
	}
	for _, input := range []string{
		`{"edits":[],"unknown":invalid}`, `{"edits":[`, `{"edits":[]} trailing`,
	} {
		if _, err := prepareEditArguments(json.RawMessage(input)); err == nil {
			t.Errorf("fast path accepted invalid JSON %q", input)
		}
	}
}
