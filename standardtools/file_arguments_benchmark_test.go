package standardtools

import (
	"encoding/json"
	"strings"
	"testing"
)

var preparedArgumentsBenchmark json.RawMessage

func BenchmarkPrepareEditArgumentsCanonical(b *testing.B) {
	for _, test := range []struct {
		name string
		size int
	}{{"small-1000", 1000}, {"large-1MiB", 1 << 20}} {
		b.Run(test.name, func(b *testing.B) {
			input, err := json.Marshal(EditRequest{Path: "large.txt", Edits: []Edit{{
				OldText: strings.Repeat("o", test.size/2),
				NewText: strings.Repeat("n", test.size/2),
			}}})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			for b.Loop() {
				preparedArgumentsBenchmark, err = prepareEditArguments(input)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
