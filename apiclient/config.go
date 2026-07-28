package apiclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/robert-crandall/go-home-server/config"
)

// FileConfig is the on-disk shape of ~/.config/<name>.json - the standing
// settings for a tool that talks to one app. Keys are camelCase to match the
// rest of the JSON in this stack.
//
//	{
//	  "appUrl": "https://myapp.example.com",
//	  "token":  "pat_1_..."
//	}
//
// It holds a personal access token, so it should be chmod 600.
type FileConfig struct {
	// AppURL is the app origin (scheme + host + port), not the /api path.
	AppURL string `json:"appUrl"`
	// Token is a pat_ token minted by POST /api/tokens.
	Token string `json:"token"`
}

// ConfigPath returns the config file path for name: $XDG_CONFIG_HOME/<name>.json
// if XDG_CONFIG_HOME is set, else $HOME/.config/<name>.json.
//
// This deliberately does not use os.UserConfigDir, which on macOS resolves to
// ~/Library/Application Support - these tools are hand-edited, so they live in
// ~/.config on every platform.
func ConfigPath(name string) (string, error) {
	if err := validateConfigName(name); err != nil {
		return "", err
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("apiclient: locate home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, name+".json"), nil
}

// LoadFileConfig reads ~/.config/<name>.json. A missing file is not an error
// (env-only usage is fine) and yields a zero FileConfig. A file that exists but
// can't be read or parsed IS an error, so a typo surfaces as "invalid JSON in
// <path>" instead of degrading into a confusing "no token" or a silent fall back
// to localhost.
func LoadFileConfig(name string) (FileConfig, error) {
	path, err := ConfigPath(name)
	if err != nil {
		return FileConfig{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return FileConfig{}, nil
		}
		return FileConfig{}, fmt.Errorf("apiclient: read %s: %w", path, err)
	}
	var cfg FileConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A hand-written two-key file makes key typos the likeliest mistake, and
	// silently ignoring "tokne" would land right back on the confusing "no
	// token" error this loader exists to prevent.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return FileConfig{}, fmt.Errorf("apiclient: parse %s: %w", path, err)
	}
	// Decode stops at the end of the first value, so anything after it - a
	// second object, a stray bracket, a bad paste - would otherwise be dropped
	// in silence. Only EOF means the file was exactly one object.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return FileConfig{}, fmt.Errorf("apiclient: parse %s: unexpected content after the JSON object", path)
	}
	cfg.AppURL = strings.TrimSpace(cfg.AppURL)
	cfg.Token = strings.TrimSpace(cfg.Token)
	return cfg, nil
}

// FromConfig builds a Client for the app named name, reading settings from
// ~/.config/<name>.json (see FileConfig) with environment overrides. It's the
// constructor an installed MCP binary should use: launched from a desktop client
// with an arbitrary working directory, the config file is the only thing it can
// count on.
//
// Precedence, highest first:
//
//  1. A real environment variable: MCP_APP_URL, MCP_APP_TOKEN. (What a desktop
//     client's "env" block sets, or a one-off shell override.)
//  2. ~/.config/<name>.json - the standing default.
//  3. A local .env found by config.LoadDotEnv, for running from an app checkout.
//  4. DefaultBaseURL for the URL. A missing token is an error.
//
// A stale .env sits below the config file on purpose: it's a leftover of
// whichever directory you happen to be standing in, not a decision about which
// app this tool talks to.
func FromConfig(name string) (*Client, error) {
	if err := validateConfigName(name); err != nil {
		return nil, err
	}
	return fromSources(name)
}

// FromEnv builds a Client from the environment alone (plus a local .env via
// config.LoadDotEnv). It does NOT require the full app config - an API client
// has no business needing DATABASE_URL.
//
//   - MCP_APP_URL:   the app origin. Optional; defaults to DefaultBaseURL.
//   - MCP_APP_TOKEN: a pat_ token from POST /api/tokens. Required.
//
// Prefer FromConfig, which adds the ~/.config/<name>.json layer an installed
// binary needs.
func FromEnv() (*Client, error) { return fromSources("") }

// fromSources resolves settings from env, the named config file (skipped when
// name is empty), and a local .env, in that order.
func fromSources(name string) (*Client, error) {
	// Snapshot the real environment first: config.LoadDotEnv merges .env into the
	// process environment, and after that there's no way to tell the two apart.
	envURL := strings.TrimSpace(os.Getenv("MCP_APP_URL"))
	envToken := strings.TrimSpace(os.Getenv("MCP_APP_TOKEN"))

	var file FileConfig
	configPath := ""
	if name != "" {
		var err error
		if file, err = LoadFileConfig(name); err != nil {
			return nil, err
		}
		configPath, _ = ConfigPath(name)
	}

	config.LoadDotEnv()
	dotURL := strings.TrimSpace(os.Getenv("MCP_APP_URL"))
	dotToken := strings.TrimSpace(os.Getenv("MCP_APP_TOKEN"))

	token := firstNonEmpty(envToken, file.Token, dotToken)
	if token == "" {
		if configPath != "" {
			return nil, fmt.Errorf("apiclient: no API token: set %q in %s or MCP_APP_TOKEN in the environment (mint one with POST /api/tokens)", "token", configPath)
		}
		return nil, fmt.Errorf("apiclient: MCP_APP_TOKEN is required (mint one with POST /api/tokens)")
	}
	return New(firstNonEmpty(envURL, file.AppURL, dotURL, DefaultBaseURL), token), nil
}

// validateConfigName rejects names that can't be a single config file. Passing a
// full module path ("github.com/you/app") instead of its base is the realistic
// mistake, and it would otherwise silently look under ~/.config/github.com/you/.
func validateConfigName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("apiclient: config name is required (use FromEnv for env-only configuration)")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("apiclient: config name %q must be a bare name, not a path", name)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
