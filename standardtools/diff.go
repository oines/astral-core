package standardtools

import (
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

func editDiff(path, before, after string) EditDetails {
	a, b := linesWithEndings(before), linesWithEndings(after)
	m := difflib.NewMatcher(a, b)
	var display, patch strings.Builder
	fmt.Fprintf(&patch, "--- %s\n+++ %s\n", path, path)
	first := 0
	width := len(fmt.Sprint(max(len(strings.Split(before, "\n")), len(strings.Split(after, "\n")))))
	groups := m.GetGroupedOpCodes(4)
	for groupIndex, group := range groups {
		start, end := group[0], group[len(group)-1]
		if groupIndex > 0 || start.I1 > 0 {
			fmt.Fprintf(&display, " %*s ...\n", width, "")
		}
		fmt.Fprintf(&patch, "@@ -%s +%s @@\n", diffRange(start.I1, end.I2), diffRange(start.J1, end.J2))
		for _, op := range group {
			if op.Tag != 'e' && first == 0 {
				first = op.J1 + 1
			}
			if op.Tag == 'e' || op.Tag == 'd' || op.Tag == 'r' {
				prefix := byte(' ')
				if op.Tag != 'e' {
					prefix = '-'
				}
				for i := op.I1; i < op.I2; i++ {
					writeDiffLine(&patch, prefix, a[i])
					fmt.Fprintf(&display, "%c%*d %s\n", prefix, width, i+1, strings.TrimSuffix(a[i], "\n"))
				}
			}
			if op.Tag == 'i' || op.Tag == 'r' {
				for i := op.J1; i < op.J2; i++ {
					writeDiffLine(&patch, '+', b[i])
					fmt.Fprintf(&display, "+%*d %s\n", width, i+1, strings.TrimSuffix(b[i], "\n"))
				}
			}
		}
		if groupIndex == len(groups)-1 && end.I2 < len(a) {
			fmt.Fprintf(&display, " %*s ...\n", width, "")
		}
	}
	return EditDetails{Diff: strings.TrimSuffix(display.String(), "\n"), Patch: patch.String(), FirstChangedLine: first}
}

func writeDiffLine(out *strings.Builder, prefix byte, text string) {
	out.WriteByte(prefix)
	out.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		out.WriteString("\n\\ No newline at end of file\n")
	}
}

func diffRange(start, end int) string {
	if end-start == 1 {
		return fmt.Sprint(start + 1)
	}
	if end == start {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, end-start)
}
