package sqlite

import (
	"net/url"
	"strings"
)

// databaseURI uses an empty authority. A raw Windows drive path in url.URL.Path
// would otherwise serialize as file://C:%5C..., which SQLite cannot open.
func databaseURI(path, system string) url.URL {
	if system == "windows" {
		path = strings.ReplaceAll(path, `\`, "/")
		if strings.HasPrefix(path, "//?/UNC/") {
			path = "//" + path[8:]
		} else {
			path = strings.TrimPrefix(path, "//?/")
		}
		if len(path) >= 2 && path[1] == ':' {
			path = "/" + path
		}
	}
	return url.URL{Scheme: "file", Path: path}
}
