//go:build (!arm64 && !amd64) || purego

package base64ascii

func Supported() bool   { return false }
func Valid([]byte) bool { return false }
