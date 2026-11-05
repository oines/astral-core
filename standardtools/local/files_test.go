package local

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPathAliasesAndPiFilenameVariants(t *testing.T) {
	dir := t.TempDir()
	fs := &fileSystem{cwd: dir, home: filepath.Join(dir, "home")}
	fileURLPath := filepath.ToSlash(filepath.Join(dir, "file with space"))
	if runtime.GOOS == "windows" {
		fileURLPath = "/" + fileURLPath
	}
	for _, test := range []struct{ input, want string }{
		{"@nested/file.txt", filepath.Join(dir, "nested/file.txt")},
		{"~/file.txt", filepath.Join(dir, "home/file.txt")},
		{"~", filepath.Join(dir, "home")},
		{"with\u00a0space.txt", filepath.Join(dir, "with space.txt")},
		{(&url.URL{Scheme: "file", Path: fileURLPath}).String(), filepath.Join(dir, "file with space")},
	} {
		got, err := fs.ResolvePath(context.Background(), test.input, false)
		if err != nil || got.Path != test.want {
			t.Fatalf("%q -> %q %v; want %q", test.input, got.Path, err, test.want)
		}
	}
	name := "screenshot 1.00.00\u202fPM.png"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ResolvePath(context.Background(), "screenshot 1.00.00 PM.png", true)
	if err != nil || filepath.Base(got.Path) != name {
		t.Fatalf("screenshot variant: %+v %v", got, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Mkdir(filepath.Join(dir, "real"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	a, err := fs.ResolvePath(context.Background(), "real/new.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.ResolvePath(context.Background(), "alias/new.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Key != b.Key {
		t.Fatal("aliases do not share mutation queue")
	}
	if err := fs.WriteFile(context.Background(), b.Path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a.Path, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(context.Background(), filepath.Join(dir, "link"), []byte("new")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dir, "link"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("write replaced symlink")
	}
	data, _ := os.ReadFile(a.Path)
	if string(data) != "new" {
		t.Fatal("write did not reach link target")
	}
}

func TestWindowsShellDrivePathForms(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"/c", `C:\`}, {"/c/", `C:\`}, {"/c/Users/a.txt", `C:\Users\a.txt`},
		{"/mnt/d/work/file", `D:\work\file`}, {"/cygdrive/E/src", `E:\src`},
		{"/MNT/c/file", `C:\file`}, {"/CYGDRIVE/z/file", `Z:\file`},
		{"//server/share", "//server/share"}, {`/c/mixed\path`, `/c/mixed\path`},
		{"/home/user", "/home/user"}, {"/mnt/cc/file", "/mnt/cc/file"},
		{"/cygdrive/3/file", "/cygdrive/3/file"}, {"C:/file", "C:/file"},
	} {
		if got := normalizeWindowsShellPath(test.input); got != test.want {
			t.Errorf("%q -> %q, want %q", test.input, got, test.want)
		}
	}
}

func TestWindowsShellDriveAliasesResolveToSameFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows drive resolution")
	}
	dir := t.TempDir()
	volume := filepath.VolumeName(dir)
	if len(volume) != 2 || volume[1] != ':' {
		t.Skip("temporary directory is not on a drive-letter volume")
	}
	path := filepath.Join(dir, "file with space.txt")
	if err := os.WriteFile(path, []byte("same file"), 0600); err != nil {
		t.Fatal(err)
	}
	fs := &fileSystem{cwd: dir}
	reference, err := fs.ResolvePath(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	drive := strings.ToLower(volume[:1])
	suffix := filepath.ToSlash(path[len(volume):])
	for _, prefix := range []string{"/", "/mnt/", "/cygdrive/"} {
		alias := prefix + drive + suffix
		resolved, err := fs.ResolvePath(context.Background(), alias, false)
		if err != nil || resolved.Path != reference.Path || resolved.Key != reference.Key {
			t.Fatalf("%q resolved to %+v %v; want %+v", alias, resolved, err, reference)
		}
		data, err := fs.ReadFile(context.Background(), resolved.Path)
		if err != nil || string(data) != "same file" {
			t.Fatalf("drive alias did not read original file: %q %v", data, err)
		}
	}
}

func TestFileWritesPreserveHardLinksAndParentDirectoryUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission and hard-link semantics")
	}
	dir := t.TempDir()
	fs := &fileSystem{cwd: dir}
	reference := filepath.Join(dir, "reference")
	if err := os.Mkdir(reference, 0777); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(dir, "nested", "parent")
	if err := fs.MkdirAll(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	referenceInfo, _ := os.Stat(reference)
	for _, path := range []string{filepath.Dir(parent), parent} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != referenceInfo.Mode().Perm() {
			t.Fatalf("directory mode %s: %v %v; want 0777 subject to current umask", path, info, err)
		}
	}
	file, alias := filepath.Join(parent, "file"), filepath.Join(parent, "alias")
	if err := os.WriteFile(file, []byte("old"), 0640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(file)
	if err := os.Link(file, alias); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(context.Background(), alias, []byte("new")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(file)
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "new" || !os.SameFile(before, after) || after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("in-place hardlink/permissions changed: %q %v", data, err)
	}
}
