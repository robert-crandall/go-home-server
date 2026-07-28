// Package server bootstraps the HTTP layer shared by every app: a chi router,
// a huma API (so handlers are typed and the OpenAPI spec is generated from Go),
// serving of an embedded SPA with a deep-link fallback, and graceful shutdown.
//
// The typical app flow:
//
//	srv := server.New(server.Options{
//	    Title: "My App", Version: "1.0.0", Addr: cfg.Addr,
//	    SPA: web.Dist, Middlewares: []func(http.Handler) http.Handler{authsvc.Middleware},
//	})
//	authsvc.Register(srv.API)
//	// ... register app operations on srv.API ...
//	srv.Run(ctx)
package server

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func init() {
	// Go's default mime table lacks these, which browsers care about for PWAs.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	_ = mime.AddExtensionType(".js", "text/javascript")
}

// Options configures a Server.
type Options struct {
	Title   string
	Version string
	Addr    string

	// SPA is the built frontend (the dist directory) to serve. When nil, no
	// static serving is wired up (useful for API-only services or tests).
	SPA fs.FS

	// Middlewares are chi middlewares applied to every route, e.g. an auth
	// middleware that resolves the current user onto the request context.
	Middlewares []func(http.Handler) http.Handler

	// HealthCheck, when set, is run by the GET /healthz handler to confirm the
	// app is ready (typically a database ping). Returning a non-nil error makes
	// /healthz report 503. When nil, /healthz is still mounted but only reports
	// liveness (a 200 that the process is up). It's kept off the /api tree since
	// it isn't part of the app's typed contract.
	HealthCheck func(context.Context) error
}

// Server bundles the router and huma API so apps can register operations.
type Server struct {
	Router *chi.Mux
	API    huma.API
	addr   string
}

// New builds the router and huma API. Register operations on Server.API before
// calling Run.
func New(opts Options) *Server {
	r := chi.NewMux()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	for _, m := range opts.Middlewares {
		r.Use(m)
	}

	title := opts.Title
	if title == "" {
		title = "App"
	}
	version := opts.Version
	if version == "" {
		version = "0.0.0"
	}

	api := humachi.New(r, huma.DefaultConfig(title, version))

	// A liveness/readiness probe for load balancers, uptime monitors, and
	// container healthchecks. Always mounted (never left to the SPA fallback,
	// which would answer 200 text/html and mask an outage) and kept off /api
	// since it isn't part of the typed contract.
	r.Get("/healthz", healthHandler(opts.HealthCheck))

	// Unknown /api paths return a JSON 404; everything else falls back to the
	// SPA's index.html (when an SPA is configured) so client-side deep links
	// resolve. Without this, a stale/typo'd API call would get 200 text/html.
	r.NotFound(notFoundHandler(opts.SPA))

	return &Server{Router: r, API: api, addr: opts.Addr}
}

// healthHandler reports liveness and, when a check is configured, readiness. It
// runs the check with a short timeout and returns 200 when it passes (or when
// there's no check) and 503 when it fails.
func healthHandler(check func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		code := http.StatusOK
		if check != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := check(ctx); err != nil {
				status = "degraded"
				code = http.StatusServiceUnavailable
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"status":"`+status+`","time":"`+time.Now().UTC().Format(time.RFC3339)+`"}`+"\n")
	}
}

// Run starts the server and blocks until the process receives SIGINT/SIGTERM
// or the given context is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	addr := s.addr
	if addr == "" {
		addr = ":8080"
	}
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           s.Router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// notFoundHandler routes unmatched requests. API paths get a JSON 404 so
// clients never mistake a missing endpoint for HTML; other paths fall back to
// the SPA index.html when an SPA is configured, else a plain 404.
func notFoundHandler(dist fs.FS) http.HandlerFunc {
	var spa http.HandlerFunc
	if dist != nil {
		spa = spaHandler(dist)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"title":"Not Found","status":404,"detail":"no such API endpoint"}`+"\n")
			return
		}
		if spa != nil {
			spa(w, r)
			return
		}
		http.NotFound(w, r)
	}
}

// isAPIPath reports whether a request path belongs to the JSON API, which the
// foundation always mounts under /api.
func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}

// spaHandler serves static files from dist, falling back to index.html so that
// client-side routes (deep links) resolve. This is the catch-all for anything
// the API router didn't match.
func spaHandler(dist fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(dist))
	return func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			serveIndex(w, r, dist)
			return
		}
		if f, err := dist.Open(p); err == nil {
			_ = f.Close()
			setSPACacheControl(w, p)
			fileServer.ServeHTTP(w, r)
			return
		}
		serveIndex(w, r, dist)
	}
}

// setSPACacheControl sets Cache-Control so a new deploy actually reaches clients,
// including through a CDN like Cloudflare that otherwise edge-caches .js/.css by
// file extension. p is the request path with its leading slash trimmed.
//
// It assumes a Vite-style build layout: content-hashed, immutable output lives
// under assets/ (a new build gives it a new filename), while everything else -
// index.html, the service worker, the web manifest, icons - keeps a stable name
// and so must always be revalidated. no-cache means "store but revalidate before
// use"; Cloudflare's Origin Cache Control (on by default) honors it and
// revalidates at the edge, so installed PWAs pick up the new UI without a force
// refresh.
func setSPACacheControl(w http.ResponseWriter, p string) {
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}

func serveIndex(w http.ResponseWriter, r *http.Request, dist fs.FS) {
	f, err := dist.Open("index.html")
	if err != nil {
		http.Error(w, "index.html not found in embedded SPA", http.StatusNotFound)
		return
	}
	defer f.Close()

	// index.html references the hashed bundles, so it must never be stale.
	w.Header().Set("Cache-Control", "no-cache")

	seeker, ok := f.(io.ReadSeeker)
	if !ok {
		// Should not happen for embed.FS files, but degrade gracefully.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.Copy(w, f)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", time.Time{}, seeker)
}
