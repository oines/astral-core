package astral

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompactionCandidatesShareOnlyImmutablePayload(t *testing.T) {
	image := json.RawMessage(`"YWJj"`)
	original := []Message{{Role: RoleTool, Content: []Content{{Type: "text", Text: "一二三four"}, {Type: "image", Data: image}}}, {Role: RoleUser, Content: Text("unchanged")}}
	for _, limit := range []int{4, 2, 0} {
		got := shortenedToolMessages(original, limit)
		if !strings.HasPrefix(got[0].Content[0].Text, string([]rune(original[0].Content[0].Text)[:limit])) {
			t.Fatal("rune truncation changed")
		}
		if &got[0].Content[1].Data[0] != &original[0].Content[1].Data[0] {
			t.Fatal("frozen image was copied")
		}
		got[0].Content[0].Text = "private edit"
		if original[0].Content[0].Text != "一二三four" {
			t.Fatal("history text was mutated")
		}
		if &got[1].Content[0] != &original[1].Content[0] {
			t.Fatal("unchanged content container copied")
		}
		summary := summaryMessages(got, "")
		if len(summary) != len(got)+1 || &summary[0].Content[1].Data[0] != &image[0] {
			t.Fatal("summary builder copied frozen image payload")
		}
		summary[0].Role = RoleUser
		if got[0].Role != RoleTool {
			t.Fatal("summary list did not own its message headers")
		}
	}
}
