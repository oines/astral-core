package standardtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"unicode"

	astral "github.com/oines/astral-core"
	"golang.org/x/text/unicode/norm"
)

type Edit struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

type EditRequest struct {
	Path  string `json:"path"`
	Edits []Edit `json:"edits"`
}

type EditDetails struct {
	Diff             string `json:"diff"`
	Patch            string `json:"patch"`
	FirstChangedLine int    `json:"firstChangedLine,omitempty"`
}

func (t backendTool) edit(ctx context.Context, inv astral.Invocation) (astral.ToolResult, error) {
	arguments, err := t.PrepareArguments(inv.Call.Arguments)
	if err != nil {
		return astral.ToolResult{}, err
	}
	var r EditRequest
	if err := decodeFileArguments(arguments, &r, "path"); err != nil {
		return astral.ToolResult{}, err
	}
	if len(r.Edits) == 0 {
		return astral.ToolResult{}, notExecuted("Edit tool input is invalid. edits must contain at least one replacement.")
	}
	var fields struct {
		Edits []json.RawMessage `json:"edits"`
	}
	_ = json.Unmarshal(arguments, &fields)
	for _, edit := range fields.Edits {
		if !singleEdit(edit) {
			return astral.ToolResult{}, notExecuted("invalid arguments: each edit must contain oldText and newText strings")
		}
	}
	fs, p, err := t.file(ctx, inv, r.Path, false)
	if err != nil {
		return astral.ToolResult{}, err
	}
	unlock, err := lockFile(ctx, p.Key)
	if err != nil {
		return astral.ToolResult{}, err
	}
	defer unlock()
	if err := fileAborted(ctx, astral.OutcomeNotExecuted); err != nil {
		return astral.ToolResult{}, err
	}
	data, err := fs.ReadFile(ctx, p.Path)
	if aborted := fileAborted(ctx, astral.OutcomeNotExecuted); aborted != nil {
		return astral.ToolResult{}, aborted
	}
	if err != nil {
		return astral.ToolResult{}, editAccessError(r.Path, err)
	}
	raw := decodeFileText(data)
	bom := ""
	if strings.HasPrefix(raw, "\ufeff") {
		bom, raw = "\ufeff", strings.TrimPrefix(raw, "\ufeff")
	}
	ending := "\n"
	if crlf, lf := strings.Index(raw, "\r\n"), strings.Index(raw, "\n"); crlf >= 0 && crlf < lf {
		ending = "\r\n"
	}
	original := normalizeLF(raw)
	updated, err := applyEdits(original, r.Edits, r.Path)
	if err != nil {
		return astral.ToolResult{}, notExecuted(err.Error())
	}
	if err := fileAborted(ctx, astral.OutcomeNotExecuted); err != nil {
		return astral.ToolResult{}, err
	}
	contents := bom + strings.ReplaceAll(updated, "\n", ending)
	err = fs.WriteFile(ctx, p.Path, []byte(contents))
	if aborted := fileAborted(ctx, astral.OutcomeUnknown); aborted != nil {
		return astral.ToolResult{}, aborted
	}
	if err != nil {
		return astral.ToolResult{}, err
	}
	details := editDiff(r.Path, original, updated)
	metadata, _ := json.Marshal(details)
	return astral.ToolResult{Content: astral.Text(fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(r.Edits), r.Path)), Details: metadata}, nil
}

func editAccessError(path string, err error) error {
	message := err.Error()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		message = "Error code: ENOENT"
	case errors.Is(err, fs.ErrPermission):
		message = "Error code: EACCES"
	}
	return notExecuted(fmt.Sprintf("Could not edit file: %s. %s.", path, message))
}

func normalizeLF(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// Port of Pi's normalization and original-file edit matching. Normalization is
// only a fallback; untouched lines retain their original bytes. See notices.
func fuzzyNormalize(text string) string {
	lines := strings.Split(norm.NFKC.String(text), "\n")
	for i := range lines {
		lines[i] = strings.TrimRightFunc(lines[i], func(r rune) bool {
			return unicode.Is(unicode.Zs, r) || r == '\t' || r == '\v' || r == '\f' || r == '\r' || r == '\u2028' || r == '\u2029' || r == '\ufeff'
		})
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '\u2018' && r <= '\u201b':
			return '\''
		case r >= '\u201c' && r <= '\u201f':
			return '"'
		case r >= '\u2010' && r <= '\u2015' || r == '\u2212':
			return '-'
		case r == '\u00a0' || r >= '\u2002' && r <= '\u200a' || r == '\u202f' || r == '\u205f' || r == '\u3000':
			return ' '
		default:
			return r
		}
	}, strings.Join(lines, "\n"))
}

type replacement struct {
	start, end, index int
	text              string
}

func findEdit(content, old string) (int, int, bool) {
	if at := strings.Index(content, old); at >= 0 {
		return at, len(old), false
	}
	normalizedOld := fuzzyNormalize(old)
	at := strings.Index(fuzzyNormalize(content), normalizedOld)
	return at, len(normalizedOld), at >= 0
}

func editOccurrences(content, old string) int {
	content, old = fuzzyNormalize(content), fuzzyNormalize(old)
	if old != "" {
		return strings.Count(content, old)
	}
	// JS split("") counts UTF-16 code units, and countOccurrences subtracts 1.
	count := -1
	for _, r := range content {
		count++
		if r > 0xffff {
			count++
		}
	}
	return count
}

func applyEdits(original string, edits []Edit, path string) (string, error) {
	normalized := make([]Edit, len(edits))
	fuzzy := false
	for i, e := range edits {
		normalized[i] = Edit{OldText: normalizeLF(e.OldText), NewText: normalizeLF(e.NewText)}
		if normalized[i].OldText == "" {
			if len(edits) == 1 {
				return "", fmt.Errorf("oldText must not be empty in %s.", path)
			}
			return "", fmt.Errorf("edits[%d].oldText must not be empty in %s.", i, path)
		}
		_, _, used := findEdit(original, normalized[i].OldText)
		fuzzy = fuzzy || used
	}
	base := original
	if fuzzy {
		base = fuzzyNormalize(base)
	}
	changes := make([]replacement, 0, len(edits))
	for i, e := range normalized {
		start, length, _ := findEdit(base, e.OldText)
		if start < 0 {
			if len(edits) == 1 {
				return "", fmt.Errorf("Could not find the exact text in %s. The old text must match exactly including all whitespace and newlines.", path)
			}
			return "", fmt.Errorf("Could not find edits[%d] in %s. The oldText must match exactly including all whitespace and newlines.", i, path)
		}
		if occurrences := editOccurrences(base, e.OldText); occurrences > 1 {
			if len(edits) == 1 {
				return "", fmt.Errorf("Found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique.", occurrences, path)
			}
			return "", fmt.Errorf("Found %d occurrences of edits[%d] in %s. Each oldText must be unique. Please provide more context to make it unique.", occurrences, i, path)
		}
		changes = append(changes, replacement{start: start, end: start + length, index: i, text: e.NewText})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].start < changes[j].start })
	for i := 1; i < len(changes); i++ {
		if changes[i-1].end > changes[i].start {
			return "", fmt.Errorf("edits[%d] and edits[%d] overlap in %s. Merge them into one edit or target disjoint regions.", changes[i-1].index, changes[i].index, path)
		}
	}
	result := replaceRanges(base, changes, 0)
	if fuzzy {
		var err error
		result, err = preserveUnchangedLines(original, base, changes)
		if err != nil {
			return "", err
		}
	}
	if result == original {
		if len(edits) == 1 {
			return "", fmt.Errorf("No changes made to %s. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected.", path)
		}
		return "", fmt.Errorf("No changes made to %s. The replacements produced identical content.", path)
	}
	return result, nil
}

func replaceRanges(text string, changes []replacement, offset int) string {
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		text = text[:c.start-offset] + c.text + text[c.end-offset:]
	}
	return text
}

func linesWithEndings(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func preserveUnchangedLines(original, base string, changes []replacement) (string, error) {
	originalLines, baseLines := linesWithEndings(original), linesWithEndings(base)
	if len(originalLines) != len(baseLines) {
		return "", fmt.Errorf("Cannot preserve unchanged lines because the base content has a different line count.")
	}
	starts := make([]int, len(baseLines)+1)
	for i, line := range baseLines {
		starts[i+1] = starts[i] + len(line)
	}
	type group struct {
		first, last int
		changes     []replacement
	}
	var groups []group
	for _, c := range changes {
		first := sort.Search(len(baseLines), func(i int) bool { return starts[i+1] > c.start })
		last := sort.Search(len(baseLines), func(i int) bool { return starts[i+1] >= c.end }) + 1
		if first >= len(baseLines) || last > len(baseLines) {
			return "", fmt.Errorf("Replacement range is outside the base content.")
		}
		if len(groups) > 0 && first < groups[len(groups)-1].last {
			g := &groups[len(groups)-1]
			g.last = max(g.last, last)
			g.changes = append(g.changes, c)
		} else {
			groups = append(groups, group{first, last, []replacement{c}})
		}
	}
	var result strings.Builder
	next := 0
	for _, g := range groups {
		result.WriteString(strings.Join(originalLines[next:g.first], ""))
		result.WriteString(replaceRanges(base[starts[g.first]:starts[g.last]], g.changes, starts[g.first]))
		next = g.last
	}
	result.WriteString(strings.Join(originalLines[next:], ""))
	return result.String(), nil
}
