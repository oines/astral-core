package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	protocol "github.com/oines/astral-core"
)

func fmtInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", protocol.ErrInvalid, fmt.Sprintf(format, args...))
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func copyJSON[T any](value T) (T, error) {
	if raw, ok := any(value).(json.RawMessage); ok && raw == nil {
		return value, nil
	}
	var out T
	b, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

// clone is only used on previously validated SDK-owned values.
func clone[T any](value T) T {
	out, err := copyJSON(value)
	if err != nil {
		panic(fmt.Sprintf("astral: invalid internal JSON: %v", err))
	}
	return out
}
func decodeJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmtInvalid("expected exactly one JSON value")
	}
	return v, nil
}
func sameJSON(a, b any) bool {
	x, e1 := json.Marshal(a)
	y, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && bytes.Equal(x, y)
}
