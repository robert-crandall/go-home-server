package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// TestSPACacheHeaders locks in the caching contract that makes PWA auto-update
// work through a CDN: hashed bundles are immutable, and every stable-named file
// (the HTML shell, service worker, manifest, icons) is always revalidated.
func TestSPACacheHeaders(t *testing.T) {
	dist := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html>")},
		"assets/app-abc123.js": {Data: []byte("console.log(1)")},
		"sw.js":                {Data: []byte("// sw")},
		"manifest.webmanifest": {Data: []byte("{}")},
		"icons/icon-192.png":   {Data: []byte("png")},
	}
	srv := New(Options{SPA: dist})

	cases := []struct {
		path string
		want string
	}{
		{"/assets/app-abc123.js", "public, max-age=31536000, immutable"},
		{"/sw.js", "no-cache"},
		{"/manifest.webmanifest", "no-cache"},
		{"/icons/icon-192.png", "no-cache"},
		{"/", "no-cache"},               // root -> index.html
		{"/some/deep/link", "no-cache"}, // SPA deep-link fallback -> index.html
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		srv.Router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if got := rec.Header().Get("Cache-Control"); got != c.want {
			t.Errorf("%s: Cache-Control = %q, want %q", c.path, got, c.want)
		}
	}
}
