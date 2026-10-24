//go:build !windows

package local

import (
	"errors"
	"net/url"
	"path/filepath"
)

func fileURLPath(u *url.URL) (string, error) {
	if u.Host != "" && u.Host != "localhost" {
		return "", errors.New("non-local file URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("invalid file URL")
	}
	return filepath.FromSlash(u.Path), nil
}

func pathKey(path string) string { return path }
