//go:build (arm64 || amd64) && !purego

package client

import (
	"runtime"
	"unsafe"

	moebase64 "github.com/MoeForge/moego/base64"
	"golang.org/x/sys/cpu"
)

func validatePlainBase64(encoded []byte, end int) bool {
	if runtime.GOARCH == "amd64" && !cpu.X86.HasAVX2 {
		return scalarPlainBase64(encoded, end)
	}
	// ValidateString reads synchronously and does not retain or mutate its
	// argument. This view avoids copying the immutable image payload to a string.
	view := unsafe.String(unsafe.SliceData(encoded), len(encoded))
	decoded, err := moebase64.StdEncoding.ValidateString(view)
	// The library accepts CR/LF. With the original length a multiple of four,
	// ignoring any such bytes necessarily reduces this decoded count.
	want := len(encoded)/4*3 - (len(encoded) - end)
	return err == nil && decoded == want
}
