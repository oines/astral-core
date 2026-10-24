package local

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
)

func fileURLPath(u *url.URL) (string, error) {
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("invalid file URL")
	}
	path := u.Path
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		if strings.ContainsAny(u.Host, `:\/`) || !strings.HasPrefix(path, "/") {
			return "", errors.New("invalid UNC file URL")
		}
		path = `\\` + u.Host + filepath.FromSlash(path)
	} else {
		// Standard drive URLs have a leading URI slash, not a drive-relative root.
		if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		path = filepath.FromSlash(path)
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("file URL must contain an absolute Windows path")
	}
	return path, nil
}

// Windows paths normally compare case-insensitively. Serializing the rare
// case-sensitive-directory collision is conservative: it never races an edit.
func pathKey(path string) string { return strings.ToUpper(filepath.Clean(path)) }
