package standardtools

import (
	"unicode/utf8"

	"golang.org/x/text/encoding/unicode"
)

// Node's Buffer.toString("utf-8") replaces each ill-formed maximal subpart,
// rather than merging adjacent invalid bytes as strings.ToValidUTF8 does.
// UTF8 (unlike UTF8BOM) preserves a leading BOM for read and edit to handle.
func decodeFileText(data []byte) string {
	text := string(data)
	if utf8.Valid(data) {
		return text
	}
	decoded, _ := unicode.UTF8.NewDecoder().String(text)
	return decoded
}
