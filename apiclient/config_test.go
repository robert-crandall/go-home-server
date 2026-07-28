package apiclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig points XDG_CONFIG_HOME at a temp dir and writes ~/.config/<name>.json
// there, so no test can read or clobber the real one.
func writeConfig(t *testing.T, name string, cfg FileConfig) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, name+".json")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// pinEnv sets both MCP_ vars explicitly. Setting them (even to "") also stops a
// stray .env in a parent directory from leaking into a test, since godotenv
// never overrides an already-set variable.
func pinEnv(t *testing.T, url, token string) {
	t.Helper()
	t.Setenv("MCP_APP_URL", url)
	t.Setenv("MCP_APP_TOKEN", token)
}

// unsetEnv removes both MCP_ vars for the duration of the test, so godotenv can
// actually supply them from a .env. t.Setenv can't unset, but it does register
// the restore, so set-then-unset gets both.
func unsetEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MCP_APP_URL", "")
	t.Setenv("MCP_APP_TOKEN", "")
	os.Unsetenv("MCP_APP_URL")
	os.Unsetenv("MCP_APP_TOKEN")
}

// inDotEnvDir chdirs into a temp directory holding a .env with both MCP_ vars,
// which is the "running from an app checkout" case.
func inDotEnvDir(t *testing.T, url, token string) {
	t.Helper()
	dir := t.TempDir()
	body := "MCP_APP_URL=" + url + "\nMCP_APP_TOKEN=" + token + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Chdir(dir)
}

func TestFromConfigReadsFile(t *testing.T) {
	writeConfig(t, "myapp", FileConfig{AppURL: "https://myapp.example.com", Token: "pat_1_file"})
	pinEnv(t, "", "")

	c, err := FromConfig("myapp")
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.baseURL != "https://myapp.example.com" {
		t.Errorf("baseURL = %q", c.baseURL)
	}
	if c.token != "pat_1_file" {
		t.Errorf("token = %q", c.token)
	}
}

func TestFromConfigEnvOverridesFile(t *testing.T) {
	writeConfig(t, "myapp", FileConfig{AppURL: "https://myapp.example.com", Token: "pat_1_file"})
	pinEnv(t, "http://localhost:9999", "pat_1_env")

	c, err := FromConfig("myapp")
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.baseURL != "http://localhost:9999" {
		t.Errorf("baseURL = %q, want the env override", c.baseURL)
	}
	if c.token != "pat_1_env" {
		t.Errorf("token = %q, want the env override", c.token)
	}
}

// A copied .env ships MCP_APP_TOKEN= (empty). An empty variable must not shadow
// the config file, or an installed binary run from an app checkout would break.
func TestFromConfigIgnoresEmptyEnv(t *testing.T) {
	writeConfig(t, "myapp", FileConfig{AppURL: "https://myapp.example.com", Token: "pat_1_file"})
	pinEnv(t, "", "")

	c, err := FromConfig("myapp")
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.token != "pat_1_file" || c.baseURL != "https://myapp.example.com" {
		t.Errorf("empty env shadowed the config file: baseURL=%q token=%q", c.baseURL, c.token)
	}
}

func TestFromConfigFallsBackToEnvWhenFileMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pinEnv(t, "", "pat_1_env")

	c, err := FromConfig("myapp")
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want default", c.baseURL)
	}
	if c.token != "pat_1_env" {
		t.Errorf("token = %q", c.token)
	}
}

func TestFromConfigMissingTokenNamesConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	pinEnv(t, "", "")

	_, err := FromConfig("myapp")
	if err == nil {
		t.Fatal("expected an error when no token is configured anywhere")
	}
	want := filepath.Join(dir, "myapp.json")
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the config path %q", err, want)
	}
	if !strings.Contains(err.Error(), "MCP_APP_TOKEN") {
		t.Errorf("error %q does not name the env var", err)
	}
}

// A config file with a typo must fail loudly rather than degrading into
// "no token" or a silent fall back to localhost.
func TestFromConfigRejectsInvalidJSON(t *testing.T) {
	for _, body := range []string{
		`{"token": "pat_1_x",}`,     // trailing comma
		`{"token": "pat_1_x"} {}`,   // a second top-level value
		`{"token": "pat_1_x"} oops`, // trailing garbage
		`{"token": "pat_1_x"}]`,     // a stray closing bracket
	} {
		dir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", dir)
		path := filepath.Join(dir, "myapp.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		pinEnv(t, "", "pat_1_env")

		_, err := FromConfig("myapp")
		if err == nil {
			t.Fatalf("config %q: expected an error for malformed config JSON", body)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("config %q: error %q does not name the config path %q", body, err, path)
		}
	}
}

// A misspelled key ("tokne") is the likeliest mistake in a hand-written two-key
// file, and it must not degrade into the "no token" error this loader prevents.
func TestFromConfigRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "myapp.json")
	if err := os.WriteFile(path, []byte(`{"tokne": "pat_1_x"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	pinEnv(t, "", "")

	_, err := FromConfig("myapp")
	if err == nil {
		t.Fatal("expected an error for an unknown config key")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "tokne") {
		t.Errorf("error %q should name both the config path %q and the bad key", err, path)
	}
}

// A stale .env is a leftover of whichever directory you're standing in, not a
// decision about which app the tool talks to, so the config file outranks it.
func TestFromConfigFileOutranksDotEnv(t *testing.T) {
	writeConfig(t, "myapp", FileConfig{AppURL: "https://myapp.example.com", Token: "pat_1_file"})
	inDotEnvDir(t, "http://localhost:9", "pat_1_dotenv")
	unsetEnv(t)

	c, err := FromConfig("myapp")
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.baseURL != "https://myapp.example.com" || c.token != "pat_1_file" {
		t.Errorf("got %q / %q, want the config file's values", c.baseURL, c.token)
	}
}

// With no config file, a local .env is still honored - that's the pre-existing
// "run it from the app checkout" path.
func TestFromConfigFallsBackToDotEnv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no config file in it
	inDotEnvDir(t, "http://localhost:8123", "pat_1_dotenv")
	unsetEnv(t)

	c, err := FromConfig("myapp")
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.baseURL != "http://localhost:8123" || c.token != "pat_1_dotenv" {
		t.Errorf("got %q / %q, want the .env values", c.baseURL, c.token)
	}
}

func TestFromConfigRejectsBadNames(t *testing.T) {
	pinEnv(t, "", "pat_1_env")
	for _, name := range []string{"", "   ", "github.com/you/my-app", "../escape"} {
		if _, err := FromConfig(name); err == nil {
			t.Errorf("FromConfig(%q) = nil error, want a rejection", name)
		}
	}
}

func TestConfigPathPrefersXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	got, err := ConfigPath("myapp")
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	if got != "/tmp/xdg/myapp.json" {
		t.Errorf("ConfigPath = %q", got)
	}

	// Unset: fall back to ~/.config, never os.UserConfigDir (which is
	// ~/Library/Application Support on macOS).
	t.Setenv("XDG_CONFIG_HOME", "")
	got, err = ConfigPath("myapp")
	if err != nil {
		t.Fatalf("ConfigPath: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if want := filepath.Join(home, ".config", "myapp.json"); got != want {
		t.Errorf("ConfigPath = %q, want %q", got, want)
	}
}

// FromEnv keeps its env-only contract: a config file for some app must not
// leak into a caller that never named one.
func TestFromEnvIgnoresConfigFiles(t *testing.T) {
	writeConfig(t, "myapp", FileConfig{AppURL: "https://myapp.example.com", Token: "pat_1_file"})
	pinEnv(t, "", "")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected FromEnv to require MCP_APP_TOKEN even with a config file present")
	}
}
