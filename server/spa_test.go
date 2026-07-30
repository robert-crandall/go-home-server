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
	const (
		indexBody   = "<!doctype html>"
		versionBody = `{"version":"abc123"}`
	)
	dist := fstest.MapFS{
		"index.html":                          {Data: []byte(indexBody)},
		"assets/app-abc123.js":                {Data: []byte("console.log(1)")},
		"_app/immutable/chunks/app-abc123.js": {Data: []byte("console.log(2)")},
		"_app/version.json":                   {Data: []byte(versionBody)},
		"sw.js":                               {Data: []byte("// sw")},
		"manifest.webmanifest":                {Data: []byte("{}")},
		"icons/icon-192.png":                  {Data: []byte("png")},
	}
	srv := New(Options{SPA: dist})

	cases := []struct {
		path     string
		want     string
		wantBody string
	}{
		{path: "/assets/app-abc123.js", want: "public, max-age=31536000, immutable"},
		{path: "/_app/immutable/chunks/app-abc123.js", want: "public, max-age=31536000, immutable"},
		{path: "/_app/version.json", want: "no-cache", wantBody: versionBody},
		{path: "/sw.js", want: "no-cache"},
		{path: "/manifest.webmanifest", want: "no-cache"},
		{path: "/icons/icon-192.png", want: "no-cache"},
		{path: "/", want: "no-cache", wantBody: indexBody},               // root -> index.html
		{path: "/some/deep/link", want: "no-cache", wantBody: indexBody}, // SPA deep-link fallback -> index.html
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		srv.Router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if got := rec.Header().Get("Cache-Control"); got != c.want {
			t.Errorf("%s: Cache-Control = %q, want %q", c.path, got, c.want)
		}
		if c.wantBody != "" && rec.Body.String() != c.wantBody {
			t.Errorf("%s: body = %q, want %q", c.path, rec.Body.String(), c.wantBody)
		}
	}
}
