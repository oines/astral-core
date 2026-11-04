package client

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unsafe"
)

type executionImage struct {
	marker   []byte
	value    string
	position int
}

const executionImagePrefix = `"\u0000astral-image-`

// marshalExecutionBody leaves JSON syntax, key ordering, escaping and opaque
// values to encoding/json. Only large, verified ASCII image strings are spliced
// into one final allocation, avoiding the encoder's large buffer and final copy.
func marshalExecutionBody(body map[string]any) ([]byte, error) {
	var images []executionImage
	metadata, changed, supported := executionImageMetadata(body, &images, 0)
	if !supported || !changed {
		return json.Marshal(body)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	if !locateExecutionImages(raw, images) {
		return json.Marshal(body)
	}
	size := len(raw)
	for i := range images {
		image := &images[i]
		extra := len(image.value) + 2 - len(image.marker)
		if extra > int(^uint(0)>>1)-size {
			return json.Marshal(body)
		}
		size += extra
	}
	sort.Slice(images, func(i, j int) bool { return images[i].position < images[j].position })
	result := make([]byte, 0, size)
	position := 0
	for _, image := range images {
		result = append(result, raw[position:image.position]...)
		result = append(result, '"')
		result = append(result, image.value...)
		result = append(result, '"')
		position = image.position + len(image.marker)
	}
	return append(result, raw[position:]...), nil
}

// Scan the metadata once for all image positions. A user-controlled identical
// string/key causes fallback instead of interpreting an ambiguous marker.
func locateExecutionImages(raw []byte, images []executionImage) bool {
	for i := range images {
		images[i].position = -1
	}
	for offset := 0; offset < len(raw); {
		at := bytes.Index(raw[offset:], []byte(executionImagePrefix))
		if at < 0 {
			break
		}
		at += offset
		digits := at + len(executionImagePrefix)
		end := digits
		for end < len(raw) && raw[end] >= '0' && raw[end] <= '9' {
			end++
		}
		index, err := strconv.Atoi(string(raw[digits:end]))
		if err == nil && index >= 0 && index < len(images) && bytes.HasPrefix(raw[at:], images[index].marker) {
			if images[index].position >= 0 {
				return false
			}
			images[index].position = at
		}
		offset = digits
	}
	for _, image := range images {
		if image.position < 0 {
			return false
		}
	}
	return true
}

// This is only the provider builders' map/array vocabulary. Unsupported Go
// values or deep/cyclic containers fall back before invoking any MarshalJSON.
func executionImageMetadata(value any, images *[]executionImage, depth int) (any, bool, bool) {
	if depth > 64 {
		return nil, false, false
	}
	switch value := value.(type) {
	case map[string]any:
		var copy map[string]any
		for key, child := range value {
			projected, changed, supported := executionImageMetadata(child, images, depth+1)
			if !supported {
				return nil, false, false
			}
			if changed {
				if copy == nil {
					copy = make(map[string]any, len(value))
					for k, v := range value {
						copy[k] = v
					}
				}
				copy[key] = projected
			}
		}
		if copy != nil {
			return copy, true, true
		}
		return value, false, true
	case []any:
		var copy []any
		for i, child := range value {
			projected, changed, supported := executionImageMetadata(child, images, depth+1)
			if !supported {
				return nil, false, false
			}
			if changed {
				if copy == nil {
					copy = append([]any(nil), value...)
				}
				copy[i] = projected
			}
		}
		if copy != nil {
			return copy, true, true
		}
		return value, false, true
	case string:
		if !executionASCIIImageURL(value) {
			return value, false, true
		}
		marker := strconv.AppendInt([]byte(executionImagePrefix), int64(len(*images)), 10)
		marker = append(marker, `\u0000"`...)
		*images = append(*images, executionImage{marker: marker, value: value})
		return json.RawMessage(marker), true, true
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr, float32, float64, json.Number, json.RawMessage, []byte, []string:
		return value, false, true
	default:
		return nil, false, false
	}
}

func executionASCIIImageURL(value string) bool {
	const prefix = "data:image/"
	const separator = ";base64,"
	if len(value) < 1024 || !strings.HasPrefix(value, prefix) {
		return false
	}
	end := strings.Index(value[len(prefix):min(len(value), len(prefix)+64)], separator)
	if end <= 0 {
		return false
	}
	end += len(prefix)
	for _, c := range value[len(prefix):end] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '+' || c == '-') {
			return false
		}
	}
	payload := value[end+len(separator):]
	// isPlainBase64 reads synchronously and does not retain this immutable view.
	return isPlainBase64(unsafe.Slice(unsafe.StringData(payload), len(payload)))
}
