package local

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	astral "github.com/oines/astral-core"
	"github.com/oines/astral-core/standardtools"
	"golang.org/x/text/unicode/norm"
)

type fileSystem struct{ cwd, home string }

// Path handling mirrors Pi: @ prefixes, file URLs, tilde, pasted spaces and
// macOS screenshot/Unicode filename variants. Authorization belongs to the host.
func expandPath(path, cwd, home string) (string, error) {
	path = strings.TrimPrefix(path, "@")
	path = strings.Map(func(r rune) rune {
		if r == '\u00a0' || r >= '\u2000' && r <= '\u200a' || r == '\u202f' || r == '\u205f' || r == '\u3000' {
			return ' '
		}
		return r
	}, path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home == "" {
			return "", errors.New("home directory is unavailable")
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if strings.HasPrefix(path, "file://") {
		u, err := url.Parse(path)
		if err != nil {
			return "", err
		}
		if u.Host != "" && u.Host != "localhost" {
			return "", errors.New("non-local file URL")
		}
		path = filepath.FromSlash(u.Path)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path), nil
}

func (fs *fileSystem) ResolvePath(ctx context.Context, path string, forRead bool) (standardtools.ResolvedPath, error) {
	if err := ctx.Err(); err != nil {
		return standardtools.ResolvedPath{}, beforeStart(err)
	}
	resolved, err := expandPath(path, fs.cwd, fs.home)
	if err != nil {
		return standardtools.ResolvedPath{}, err
	}
	if forRead {
		// Pi retries filesystem spelling variations only for read.
		nfd := norm.NFD.String(resolved)
		variants := []string{resolved, screenshotTime.ReplaceAllString(resolved, "\u202f$1."), nfd, strings.ReplaceAll(resolved, "'", "\u2019"), strings.ReplaceAll(nfd, "'", "\u2019")}
		for _, variant := range variants {
			if _, err := os.Stat(variant); err == nil {
				resolved = variant
				break
			}
		}
	}
	canonical, err := canonicalPath(resolved)
	if err != nil {
		return standardtools.ResolvedPath{}, err
	}
	return standardtools.ResolvedPath{Path: resolved, Parent: filepath.Dir(resolved), Key: "local:" + canonical}, nil
}

var screenshotTime = regexp.MustCompile(`(?i) (AM|PM)\.`)

// Resolve the nearest existing ancestor too, so two aliases for a file that
// does not exist yet share the same mutation queue.
func canonicalPath(path string) (string, error) {
	current := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func (*fileSystem) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, beforeStart(err)
	}
	data, err := os.ReadFile(path)
	if err == nil {
		err = ctx.Err()
	}
	return data, err
}

func (*fileSystem) MkdirAll(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return beforeStart(err)
	}
	return os.MkdirAll(path, 0755)
}

// Like Pi, writes follow symlinks, preserve existing permissions and update
// hard-linked files in place. The SDK validates every edit before calling this.
// An IO failure after truncation may have partially mutated the file.
func (*fileSystem) WriteFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return beforeStart(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	n, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil || n != len(data) {
		if n != len(data) {
			writeErr = errors.Join(writeErr, errors.New("short write"))
		}
		return &astral.ToolError{Outcome: astral.OutcomeUnknown, Code: "partial_write", Cause: fmt.Errorf("file may have been partially written: %w", errors.Join(writeErr, closeErr))}
	}
	return nil
}
