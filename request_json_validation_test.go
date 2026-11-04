package astral

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

func TestRequestJSONFastStringMatchesStandard(t *testing.T) {
	cases := [][]byte{[]byte(`""`), []byte(`"a<>&"`), []byte(`"escaped\n\u0000\ud800"`), []byte(`"x" false`), []byte(`"x"\n`), []byte(`"x" `), []byte(`"x`), []byte(`null`), []byte(strings.Repeat("[", 10001) + strings.Repeat("]", 10001))}
	for b := range 256 {
		cases = append(cases, []byte{'"', byte(b), '"'})
	}
	rng := rand.New(rand.NewSource(90141))
	for range 10000 {
		raw := make([]byte, 2+rng.Intn(80))
		rng.Read(raw)
		raw[0] = '"'
		raw[len(raw)-1] = '"'
		cases = append(cases, raw)
	}
	for i, raw := range cases {
		if got, want := validRequestJSON(raw), json.Valid(raw); got != want {
			t.Fatalf("case %d: fast=%t std=%t raw=%q", i, got, want, raw)
		}
	}
}
