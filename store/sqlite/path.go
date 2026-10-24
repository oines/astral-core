package sqlite

import (
	"errors"
	"os"
	"path/filepath"
)

// databasePath resolves aliases before either SQLite connection is opened.
// Windows SQLite does not resolve file symlinks itself: an alias DSN otherwise
// looks for a different -wal/-shm pair and can miss an existing database's schema.
// The same canonical name also scopes in-process detached-fork coordination.
func databasePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// A file symlink may point at a database that has not been created yet.
	// Follow its intended target before preparing its parent directory.
	if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(abs)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(abs), target)
		}
		return databasePath(target)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return "", err
	}
	// New database files have no final component to resolve yet. Resolve the
	// prepared parent so the very first WAL uses the same path as later handles.
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}
