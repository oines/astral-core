//go:build (!arm64 && !amd64) || purego

package client

func validatePlainBase64(encoded []byte, end int) bool {
	return scalarPlainBase64(encoded, end)
}
