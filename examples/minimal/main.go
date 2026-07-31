// Command minimal is the smallest complete app built on this foundation: it
// loads config, applies the shared migrations, wires auth, web push, and file
// uploads onto one huma API, and serves it.
//
// It is deliberately API-only and feature-free - it exists to show the wiring,
// and it is compiled by CI so the wiring can't drift out of date. Copy it,
// then add your own migrations, endpoints, and SPA.
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/robert-crandall/go-home-server/auth"
	"github.com/robert-crandall/go-home-server/config"
	"github.com/robert-crandall/go-home-server/db"
	"github.com/robert-crandall/go-home-server/files"
	"github.com/robert-crandall/go-home-server/migrations"
	"github.com/robert-crandall/go-home-server/notify"
	"github.com/robert-crandall/go-home-server/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx := context.Background()

	// Each migration source tracks its own goose version table, so an app's own
	// migrations can also start at 00001. A real app adds a second source:
	//
	//	db.MigrationSource{FS: myapp.MigrationsFS, Dir: "migrations"}
	if err := db.Migrate(cfg.DatabaseURL, db.MigrationSource{
		FS:        migrations.FS,
		Dir:       migrations.Dir,
		TableName: migrations.TableName,
	}); err != nil {
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

	// A missing or unwritable upload directory is fatal on purpose: the
	// alternative is writing uploads to a container layer that gets thrown away
	// on the next deploy.
	filesSvc, err := files.NewService(pool, files.Options{
		Dir:      cfg.UploadDir,
		MaxBytes: cfg.UploadMaxBytes,
	})
	if err != nil {
		log.Fatal(err)
	}

	srv := server.New(server.Options{
		Title:   "Minimal App",
		Version: "1.0.0",
		Addr:    cfg.Addr,
		// SPA is nil here, so nothing static is served. An app with a frontend
		// passes its embedded dist directory instead: SPA: web.Dist.
		Middlewares: []func(http.Handler) http.Handler{authSvc.Middleware},
		HealthCheck: pool.Ping,
		HumaConfig:  authSvc.TokenHumaConfig,
	})

	// Register the foundation's operations on the shared huma API.
	authSvc.Register(srv.API)
	authSvc.RegisterTokens(srv.API) // /api/tokens + bearer auth for scripts/MCP

	// Optional: "Sign in with Google" alongside the password login. Skipped
	// entirely when no client is configured, so a password-only app needs no
	// Google setup at all.
	if cfg.GoogleClientID != "" {
		if err := authSvc.RegisterGoogle(srv.API, auth.GoogleConfig{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  cfg.GoogleRedirectURL,
		}); err != nil {
			log.Fatal(err)
		}
	}

	currentUser := func(ctx context.Context) (int64, error) {
		u, err := auth.RequireUser(ctx)
		return u.ID, err
	}
	notify.Register(srv.API, notifySvc, currentUser)
	files.Register(srv.API, filesSvc, currentUser)

	log.Printf("listening on %s (env=%s)", cfg.Addr, cfg.Env)
	if err := srv.Run(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
}
