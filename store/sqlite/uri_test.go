package sqlite

import "testing"

func TestDatabaseURI(t *testing.T) {
	for _, test := range []struct{ system, path, want string }{
		{"windows", `C:\Users\中文 folder\core.db`, "file:///C:/Users/%E4%B8%AD%E6%96%87%20folder/core.db"},
		{"windows", `\\server\share\core.db`, "file:////server/share/core.db"},
		{"windows", `\\?\C:\long\core.db`, "file:///C:/long/core.db"},
		{"windows", `\\?\UNC\server\share\core.db`, "file:////server/share/core.db"},
		{"linux", "/tmp/name #?.db", "file:///tmp/name%20%23%3F.db"},
	} {
		uri := databaseURI(test.path, test.system)
		if uri.String() != test.want {
			t.Errorf("%s %q = %q, want %q", test.system, test.path, uri.String(), test.want)
		}
	}
}
