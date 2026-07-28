// Command server runs the example app: it applies migrations, wires the
// foundation's auth, notifications, and HTTP server, mounts the sample notes
// feature, and serves the embedded SPA.
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/robert-crandall/go-home-server/auth"
	"github.com/robert-crandall/go-home-server/config"
	"github.com/robert-crandall/go-home-server/db"
	"github.com/robert-crandall/go-home-server/notify"
	"github.com/robert-crandall/go-home-server/server"

	"github.com/robert-crandall/example-app/internal/app"
	"github.com/robert-crandall/example-app/internal/notes"
	"github.com/robert-crandall/example-app/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx := context.Background()

	if err := app.Migrate(cfg.DatabaseURL); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	pool, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	authSvc := auth.NewService(pool, cfg.IsProduction())
	authSvc.OpenRegistration = cfg.AllowOpenRegistration
	notifySvc, err := notify.NewService(pool, notify.VAPID{
		Public:  cfg.VAPIDPublic,
		Private: cfg.VAPIDPrivate,
		Subject: cfg.VAPIDSubject,
	})
	if err != nil {
		log.Fatalf("notify: %v", err)
	}
	notesSvc := notes.NewService(pool, notifySvc)

	srv := server.New(server.Options{
		Title:       "Example App",
		Version:     "1.0.0",
		Addr:        cfg.Addr,
		SPA:         web.Dist,
		Middlewares: []func(http.Handler) http.Handler{authSvc.Middleware},
		HealthCheck: pool.Ping,
	})

	// Register API operations on the shared huma API.
	authSvc.Register(srv.API)
	authSvc.RegisterTokens(srv.API) // /api/tokens + bearer auth for scripts/MCP
	notify.Register(srv.API, notifySvc, func(ctx context.Context) (int64, error) {
		u, err := auth.RequireUser(ctx)
		return u.ID, err
	})
	notesSvc.Register(srv.API)

	log.Printf("listening on %s (env=%s)", cfg.Addr, cfg.Env)
	if err := srv.Run(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
}
