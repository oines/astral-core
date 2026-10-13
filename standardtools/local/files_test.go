package local

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPathAliasesAndPiFilenameVariants(t *testing.T) {
	dir := t.TempDir()
	fs := &fileSystem{cwd: dir, home: filepath.Join(dir, "home")}
	for _, test := range []struct{ input, want string }{
		{"@nested/file.txt", filepath.Join(dir, "nested/file.txt")},
		{"~/file.txt", filepath.Join(dir, "home/file.txt")},
		{"~", filepath.Join(dir, "home")},
		{"with\u00a0space.txt", filepath.Join(dir, "with space.txt")},
		{(&url.URL{Scheme: "file", Path: filepath.Join(dir, "file with space")}).String(), filepath.Join(dir, "file with space")},
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
