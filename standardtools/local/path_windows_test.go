package local

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestWindowsFileURLsAndHomePaths(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"file:///C:/Users/Owner/My%20Files/中文.txt", `C:\Users\Owner\My Files\中文.txt`},
		{"file://localhost/C:/Projects/test.txt", `C:\Projects\test.txt`},
		{"file://server/share/dir/file.txt", `\\server\share\dir\file.txt`},
		{`~\folder\file.txt`, `C:\Users\Owner\folder\file.txt`},
		{`C:/Projects/test.txt`, `C:\Projects\test.txt`},
	} {
		got, err := expandPath(test.input, `C:\Projects`, `C:\Users\Owner`)
		if err != nil || got != test.want {
			t.Errorf("%q => %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	for _, raw := range []string{"file:///relative/path", "file:///C:/file?query=1", "file://user@host/share/file", "file:///C:/file#fragment"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fileURLPath(u); err == nil {
			t.Errorf("accepted invalid file URL %q", raw)
		}
	}
}

func TestWindowsPathAliasesShareMutationQueue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MixedCase.txt")
	if err := os.WriteFile(path, []byte("text"), 0600); err != nil {
		t.Fatal(err)
	}
	fs := fileSystem{cwd: dir, home: dir}
	a, err := fs.ResolvePath(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.ResolvePath(context.Background(), strings.ToUpper(path), false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Key != b.Key {
		t.Fatalf("aliases have different keys: %q vs %q", a.Key, b.Key)
	}
}

func TestWindowsEnvironmentCaseOverridesAndUnicode(t *testing.T) {
	merged := mergeEnv([]string{"Path=old", "HOME=old", `=C:=C:\old`}, []string{"PATH=new", "Home=新目录", `=C:=C:\new`})
	if len(merged) != 3 || merged[0] != "PATH=new" || merged[1] != "Home=新目录" || merged[2] != `=C:=C:\new` {
		t.Fatalf("case-insensitive environment merge: %q", merged)
	}
	block, err := windowsEnvironment(merged)
	if err != nil {
		t.Fatal(err)
	}
	if len(block) < 2 || block[len(block)-1] != 0 || block[len(block)-2] != 0 {
		t.Fatalf("environment is not double-NUL terminated: %v", block)
	}
	if text := string(utf16.Decode(block)); !strings.Contains(text, "Home=新目录\x00") {
		t.Fatalf("Unicode environment lost: %q", text)
	}
	if _, err := windowsEnvironment([]string{"INVALID=nul\x00value"}); err == nil {
		t.Fatal("accepted environment NUL")
	}
}
