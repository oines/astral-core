package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenExistingFileAliasReadsActiveWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "中文 active history.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	state, _ := fixture(t, first)
	if info, err := os.Stat(first.path + "-wal"); err != nil || info.Size() <= 32 {
		t.Fatalf("fixture has no active WAL: %v", err)
	}
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err = os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	second, err := Open(alias)
	if err != nil {
		t.Fatalf("opening alias missed the live database format in its WAL: %v", err)
	}
	defer second.Close()
	if first.path != second.path {
		t.Fatalf("handles use different canonical identities: %q != %q", first.path, second.path)
	}
	loaded, err := second.LoadState(context.Background(), state.SessionID)
	if err != nil || loaded.Version != state.Version {
		t.Fatalf("alias could not read committed WAL state: %+v %v", loaded, err)
	}
	if _, err = os.Stat(alias + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("alias created a separate WAL sidecar: %v", err)
	}
}

func TestOpenNewDatabaseThroughDirectoryAlias(t *testing.T) {
	directory := t.TempDir()
	alias := filepath.Join(t.TempDir(), "directory-alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	first, err := Open(filepath.Join(alias, "new-directory", "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	state, _ := fixture(t, first)
	actual := filepath.Join(directory, "new-directory", "history.db")
	canonical, err := filepath.EvalSymlinks(actual)
	if err != nil || canonical != first.path {
		t.Fatalf("initial database path did not resolve its parent alias: %q %q %v", first.path, canonical, err)
	}
	second, err := Open(actual)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if loaded, err := second.LoadState(context.Background(), state.SessionID); err != nil || loaded.Version != state.Version {
		t.Fatalf("real path cannot see the alias-created WAL: %+v %v", loaded, err)
	}
}

func TestOpenNewDatabaseThroughDanglingFileAlias(t *testing.T) {
	directory := t.TempDir()
	alias := filepath.Join(directory, "alias.db")
	if err := os.Symlink("future.db", alias); err != nil {
		t.Fatal(err)
	}
	first, err := Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	actual, err := filepath.EvalSymlinks(filepath.Join(directory, "future.db"))
	if err != nil || first.path != actual {
		t.Fatalf("new file alias did not use the intended database target: %q %q %v", first.path, actual, err)
	}
	second, err := Open(actual)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
}
