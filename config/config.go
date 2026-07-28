// Package config loads application configuration from the environment.
//
// It follows 12-factor conventions: a local .env file is loaded on a
// best-effort basis for development, but real environment variables always
// win in production. DATABASE_URL is the one required value.
package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// Config is the resolved runtime configuration shared by every app built on
// this foundation. Apps can embed it in their own larger config struct.
type Config struct {
	// DatabaseURL is the Postgres connection string (required).
	DatabaseURL string
	// Addr is the listen address for the HTTP server, e.g. ":8080".
	Addr string
	// SessionSecret is reserved for signing/derivation if an app needs it.
	SessionSecret string
	// Env is "development" or "production". Controls the Secure cookie flag.
	Env string

	// VAPID keys for web push. Optional; notifications are disabled when unset.
	VAPIDPublic  string
	VAPIDPrivate string
	VAPIDSubject string

	// AllowOpenRegistration lets anyone register. Default false, which means
	// registration is first-user-only (the app bootstraps a single user, then
	// closes). Set ALLOW_OPEN_REGISTRATION=true for a genuinely multi-user app.
	AllowOpenRegistration bool

	// UploadDir is the directory file uploads are written to. It has no
	// default on purpose: in production this is a bind-mounted host directory,
	// and guessing a path would mean silently writing to the container's
	// ephemeral layer. An app that never constructs files.NewService can leave
	// it empty; one that does - including the template, which always registers
	// the file routes - should treat empty as a fatal misconfiguration.
	UploadDir string
	// UploadMaxBytes caps a single upload. 0 uses the files package default.
	UploadMaxBytes int64
}

// Load reads configuration from the environment (plus an optional .env file).
func Load() (Config, error) {
	LoadDotEnv() // best-effort; absent .env is fine

	c := Config{
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		Addr:                  getenv("ADDR", ":8080"),
		SessionSecret:         os.Getenv("SESSION_SECRET"),
		Env:                   getenv("APP_ENV", "development"),
		VAPIDPublic:           os.Getenv("VAPID_PUBLIC_KEY"),
		VAPIDPrivate:          os.Getenv("VAPID_PRIVATE_KEY"),
		VAPIDSubject:          getenv("VAPID_SUBJECT", "mailto:admin@example.com"),
		AllowOpenRegistration: os.Getenv("ALLOW_OPEN_REGISTRATION") == "true",
		UploadDir:             os.Getenv("UPLOAD_DIR"),
		UploadMaxBytes:        getenvInt64("UPLOAD_MAX_BYTES"),
	}

	if c.DatabaseURL == "" {
		return c, fmt.Errorf("config: DATABASE_URL is required")
	}
	return c, nil
}

// IsProduction reports whether the app is running in production mode.
func (c Config) IsProduction() bool { return c.Env == "production" }

// LoadDotEnv looks for a .env file in the working directory and a few parents,
// so a copied app with .env at its root still works when a command is run from
// a subdirectory (e.g. server/ or server/cmd/...). It's best-effort: an absent
// .env is fine, and real environment variables always win.
//
// It's exported (not just used by Load) so lightweight clients - an MCP server,
// a cron script - can honor a local .env without pulling in the full app config
// (which requires DATABASE_URL).
func LoadDotEnv() {
	for _, p := range []string{".env", "../.env", "../../.env", "../../../.env"} {
		if _, err := os.Stat(p); err == nil {
			_ = godotenv.Load(p)
			return
		}
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getenvInt64 reads an integer setting. An unset or unparseable value yields 0,
// which callers treat as "use the default" - a typo in an optional tuning knob
// shouldn't stop the app from booting.
func getenvInt64(key string) int64 {
	n, err := strconv.ParseInt(os.Getenv(key), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
