package client

import (
	"bytes"
	"encoding/json"
	"strings"

	core "github.com/oines/astral-core"
)

// ExecutionImages is local to one synchronous private protocol build. Keys are
// the final COW content addresses, never user-controlled strings. Values borrow
// verified frozen pixels only until NewExecution owns the encoded body.
type ExecutionImages map[*core.Content]executionImageValue

type executionImageValue struct {
	prefix string
	data   []byte
}

// MarshalJSON preserves the standard encoder's bytes on unsupported-tree or
// marker-collision fallback. The ordinary execution path never materializes it.
func (v executionImageValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.prefix + string(v.data))
}

func (images ExecutionImages) URL(part *core.Content) any {
	if value, ok := images[part]; ok {
		return value
	}
	return part.URL
}

func (images ExecutionImages) Base64(part *core.Content) (any, bool) {
	value, ok := images[part]
	value.prefix = ""
	return value, ok
}

// imageData has already validated encoded. Only its borrowed, plain JSON-string
// result qualifies; escaped/newline inputs and unusual MIME retain the old path.
func (images ExecutionImages) deferredValue(part core.Content, encoded []byte) (executionImageValue, bool) {
	if images == nil {
		return executionImageValue{}, false
	}
	raw := bytes.Trim(part.Data, " \t\r\n")
	if len(encoded) < 1024 || len(raw) != len(encoded)+2 || &raw[1] != &encoded[0] || !executionImageMIME(part.MediaType) {
		return executionImageValue{}, false
	}
	return executionImageValue{prefix: "data:" + part.MediaType + ";base64,", data: encoded}, true
}

func executionImageMIME(media string) bool {
	if !strings.HasPrefix(media, "image/") || len(media) <= len("image/") || len(media) > len("image/")+64 {
		return false
	}
	for _, c := range media[len("image/"):] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '+' || c == '-') {
			return false
		}
	}
	return true
}
