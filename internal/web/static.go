package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var embedded embed.FS

// staticFS is the embedded static/ subtree rooted so file names resolve as
// e.g. "login.html" rather than "static/login.html". Computed once at init.
var staticFS = func() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic("web: fs.Sub(static) failed: " + err.Error())
	}
	return sub
}()
