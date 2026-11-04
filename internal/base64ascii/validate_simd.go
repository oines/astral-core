//go:build (arm64 || amd64) && !purego

// Package base64ascii recognizes standard base64 without decoded buffers.
package base64ascii

import (
	"runtime"
	"unsafe"

	moebase64 "github.com/MoeForge/moego/base64"
	"golang.org/x/sys/cpu"
)

func Supported() bool { return runtime.GOARCH != "amd64" || cpu.X86.HasAVX2 }

// Valid returns true only for plain, nonempty standard base64 with no CR/LF.
// Unsupported CPUs return false so callers can use their allocation-free
// scalar paths instead of the dependency's decoded-buffer fallback.
func Valid(encoded []byte) bool {
	if !Supported() || len(encoded) == 0 || len(encoded)%4 != 0 {
		return false
	}
	end := len(encoded)
	if encoded[end-1] == '=' {
		end--
		if encoded[end-1] == '=' {
			end--
		}
	}
	// ValidateString reads synchronously without retaining this immutable view.
	view := unsafe.String(unsafe.SliceData(encoded), len(encoded))
	decoded, err := moebase64.StdEncoding.ValidateString(view)
	// The dependency permits CR/LF; ignoring any bytes necessarily reduces
	// the decoded count relative to the original multiple-of-four length.
	want := len(encoded)/4*3 - (len(encoded) - end)
	return err == nil && decoded == want
}
