package standardtools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	astral "github.com/oines/astral-core"
)

// These expectations follow Codex af5d95f255's ordinary tool response, including
// its token marker outside the retained-content budget (not code_mode_result).
func TestCodexCommandOutputTruncation(t *testing.T) {
	for _, test := range []struct {
		name, text string
		tokens     int
		want       string
	}{
		{"empty", "", 0, ""},
		{"fits", "short", 2, "short"},
		{"ascii", "abcdefghijklmnopqrstuvwxyz", 2, "Warning: truncated output (original token count: 7)\nTotal output lines: 1\n\nabcd…5 tokens truncated…wxyz"},
		{"unicode", "你好🪐尾部", 2, "Warning: truncated output (original token count: 4)\nTotal output lines: 1\n\n你…2 tokens truncated…部"},
		{"zero", "hidden", 0, "Warning: truncated output (original token count: 2)\nTotal output lines: 1\n\n…2 tokens truncated…"},
		{"lines", "alpha\nbeta\n", 1, "Warning: truncated output (original token count: 3)\nTotal output lines: 2\n\nal…2 tokens truncated…a\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Without a collection omission, Codex counts the actual text rather
			// than trusting the independently reported original token count.
			got := limitCommandOutput(test.text, 999, 0, test.tokens)
			if got != test.want || !utf8.ValidString(got) {
				t.Fatalf("got %q; want %q", got, test.want)
			}
		})
	}
}

func TestCodexCommandOutputPreservesCollectionOmission(t *testing.T) {
	marker := "... 123456 bytes omitted ..."
	raw := "HEAD-" + strings.Repeat("a", 100) + "\n" + marker + "\nTAIL-" + strings.Repeat("z", 100)
	for _, text := range []string{raw, "remaining output"} {
		for _, tokens := range []int{0, 4, 1000} {
			got := limitCommandOutput(text, 42000, 123456, tokens)
			if strings.Count(got, marker) != 1 || strings.Contains(got, "Total output lines:") {
				t.Fatalf("collection omission hidden or misrepresented: %q", got)
			}
			if len(text) > tokens*4 && !strings.Contains(got, "original token count: 42000") {
				t.Fatalf("original collection count lost: %q", got)
			}
		}
	}
}

func TestCodexCommandResponseReservesMetadataBudget(t *testing.T) {
	var raw strings.Builder
	for line := 1; line <= 150; line++ {
		fmt.Fprintf(&raw, "%d\n", line)
	}
	set := &Set{options: Options{MaxOutputTokens: 50}}
	code := 0
	result := set.commandResult(CommandResult{ChunkID: "abc123", WallTimeSeconds: 1.25, ExitCode: &code, OriginalTokenCount: 123, Output: raw.String()}, 50, 0, astral.OutcomeSucceeded, "")
	text := result.Content[0].Text
	if len(text) > 240 || strings.Count(text, "tokens truncated") != 1 || !strings.Contains(text, "Original token count: 123") || !strings.Contains(text, "Total output lines: 150") || !strings.Contains(text, "\n1\n2\n3\n") || !strings.HasSuffix(text, "149\n150\n") {
		t.Fatalf("ordinary response did not fit its host budget: %q", text)
	}
	var details CommandResult
	if err := json.Unmarshal(result.Details, &details); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(text, "Output:\n"+details.Output) {
		t.Fatal("details differ from the model-visible output")
	}
}

func TestCodexCommandResponsePreservesEmptyCountAndProcessState(t *testing.T) {
	set := &Set{options: Options{MaxOutputTokens: 10000}}
	for _, running := range []bool{false, true} {
		command := CommandResult{ChunkID: "abc123"}
		code := 7
		if running {
			command.SessionID = 12345
		} else {
			command.ExitCode = &code
		}
		result := set.commandResult(command, 10000, 0, astral.OutcomeSucceeded, "")
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(result.Details, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["original_token_count"]) != "0" || result.IsError {
			t.Fatalf("empty count or normal process status changed: %s", result.Details)
		}
		if (fields["session_id"] != nil) != running || (fields["exit_code"] != nil) == running {
			t.Fatalf("exit and running states mixed: %s", result.Details)
		}
	}
}
