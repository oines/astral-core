package standardtools

import (
	"context"
	"encoding/json"
	"fmt"
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
	var r EditRequest
	if err := decodeArguments(inv.Call.Arguments, &r); err != nil {
		return astral.ToolResult{}, err
	}
	if len(r.Edits) == 0 {
		return astral.ToolResult{}, notExecuted("edits must contain at least one replacement")
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
	data, err := fs.ReadFile(ctx, p.Path)
	if err != nil {
		return astral.ToolResult{}, err
	}
	raw := strings.ToValidUTF8(string(data), "\ufffd")
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
		return astral.ToolResult{}, err
	}
	details := editDiff(r.Path, original, updated)
	contents := bom + strings.ReplaceAll(updated, "\n", ending)
	if err := fs.WriteFile(ctx, p.Path, []byte(contents)); err != nil {
		return astral.ToolResult{}, err
	}
	metadata, _ := json.Marshal(details)
	return astral.ToolResult{Content: astral.Text(fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(r.Edits), r.Path)), Details: metadata}, nil
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
	if normalizedOld == "" {
		return -1, 0, true
	}
	return strings.Index(fuzzyNormalize(content), normalizedOld), len(normalizedOld), true
}

func applyEdits(original string, edits []Edit, path string) (string, error) {
	normalized := make([]Edit, len(edits))
	fuzzy := false
	for i, e := range edits {
		normalized[i] = Edit{OldText: normalizeLF(e.OldText), NewText: normalizeLF(e.NewText)}
		if normalized[i].OldText == "" {
			return "", fmt.Errorf("edits[%d].oldText must not be empty in %s", i, path)
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
			return "", fmt.Errorf("could not find edits[%d] in %s; oldText must match the original file", i, path)
		}
		needle := fuzzyNormalize(e.OldText)
		if needle == "" || strings.Count(fuzzyNormalize(base), needle) > 1 {
			return "", fmt.Errorf("edits[%d] is ambiguous in %s; provide more context to make oldText unique", i, path)
		}
		changes = append(changes, replacement{start: start, end: start + length, index: i, text: e.NewText})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].start < changes[j].start })
	for i := 1; i < len(changes); i++ {
		if changes[i-1].end > changes[i].start {
			return "", fmt.Errorf("edits[%d] and edits[%d] overlap in %s; merge them or target disjoint regions", changes[i-1].index, changes[i].index, path)
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
		return "", fmt.Errorf("no changes made to %s; replacements produced identical content", path)
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
		return "", fmt.Errorf("normalization changed line count; supply an exact match")
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
			return "", fmt.Errorf("edit lies outside normalized file")
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
