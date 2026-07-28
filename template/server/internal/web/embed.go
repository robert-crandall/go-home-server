// Package web embeds the built Svelte SPA so the Go binary is fully
// self-contained. The Vite build writes into dist/; a fallback index.html is
// committed so the package always compiles even before the frontend is built.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var distFS embed.FS

// Dist is the built SPA (the contents of dist/), ready for server.Options.SPA.
var Dist fs.FS

func init() {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	Dist = sub
}
