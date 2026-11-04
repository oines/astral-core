// Package jsonstring recognizes content that encoding/json writes unchanged
// between quotes, including its default HTML escaping policy.
package jsonstring

import "encoding/binary"

// UnescapedASCII reports whether value needs neither JSON nor HTML escaping.
// It reads without retaining or changing the borrowed value.
func UnescapedASCII[T ~string | ~[]byte](raw T) bool {
	// Check eight ASCII bytes at a time. Any control, non-ASCII, JSON escape
	// or HTML-sensitive byte keeps the established encoder fallback below.
	const high = uint64(0x8080808080808080)
	const low = uint64(0x0101010101010101)
	for len(raw) >= 8 {
		x := binary.LittleEndian.Uint64([]byte(raw[:8]))
		if x&high != 0 || (x-0x2020202020202020)&^x&high != 0 {
			return false
		}
		quote, slash := x^0x2222222222222222, x^0x5c5c5c5c5c5c5c5c
		lt, gt, amp := x^0x3c3c3c3c3c3c3c3c, x^0x3e3e3e3e3e3e3e3e, x^0x2626262626262626
		if ((quote-low)&^quote|(slash-low)&^slash|(lt-low)&^lt|(gt-low)&^gt|(amp-low)&^amp)&high != 0 {
			return false
		}
		raw = raw[8:]
	}
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if b < 0x20 || b >= 0x80 || b == '"' || b == '\\' || b == '<' || b == '>' || b == '&' {
			return false
		}
	}
	return true
}
