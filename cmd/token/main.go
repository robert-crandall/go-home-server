// Command token mints a personal access token for an app built on this
// foundation, using nothing but the app's public HTTP API: it logs in with your
// password exactly as the browser does, then calls POST /api/tokens.
//
// It exists because /api/tokens is session-only (see auth.RequireSessionUser),
// so a token can't bootstrap another token - which otherwise leaves "get a
// token" as a manual click-through before an MCP server or script can talk to
// anything. Going through HTTP rather than the database also means this works
// from any machine against a deployed app, and reuses the app's real auth path.
//
//	go run github.com/robert-crandall/go-home-server/cmd/token \
//	  -url https://my-app.example.com -email you@example.com \
//	  -name my-app-mcp -config my-app
//
// Without -config the plaintext token is printed on stdout and nothing else, so
// it composes with a pipeline; every prompt and diagnostic goes to stderr.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robert-crandall/go-home-server/apiclient"
	"golang.org/x/term"
)

// requestTimeout bounds the whole login + mint exchange, so a wedged app fails
// fast instead of hanging a terminal.
const requestTimeout = 30 * time.Second

// maxErrorBodyBytes caps how much of a non-2xx body is quoted back, so an app
// serving an HTML error page doesn't flood the terminal.
const maxErrorBodyBytes = 4 << 10 // 4 KiB

func main() {
	log.SetFlags(0)
	log.SetPrefix("token: ")

	appURL := flag.String("url", firstNonEmpty(os.Getenv("MCP_APP_URL"), apiclient.DefaultBaseURL),
		"App origin (scheme://host:port), not the /api path. Defaults to $MCP_APP_URL.")
	email := flag.String("email", os.Getenv("APP_EMAIL"),
		"Email of the account to mint the token for. Defaults to $APP_EMAIL.")
	name := flag.String("name", "cli",
		"Display label for the token, so you can tell tokens apart later")
	configName := flag.String("config", "",
		"Bare app name: write ~/.config/<name>.json (mode 0600) instead of printing the token")
	flag.Parse()

	if strings.TrimSpace(*email) == "" {
		log.Fatal("-email is required (or set APP_EMAIL)")
	}

	password, err := readPassword(*email)
	if err != nil {
		log.Fatal(err)
	}

	client, err := newClient()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	token, err := mint(ctx, client, *appURL, *email, password, *name)
	if err != nil {
		log.Fatal(err)
	}

	if *configName == "" {
		fmt.Println(token)
		return
	}
	path, err := writeConfig(*configName, *appURL, token)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
}

// newClient returns an HTTP client with a cookie jar. The jar is the whole
// point: login hands back the session cookie that POST /api/tokens requires.
func newClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookie jar: %w", err)
	}
	return &http.Client{Jar: jar, Timeout: requestTimeout}, nil
}

// mint logs in and creates a token, returning the plaintext - which the app
// returns exactly once and can never show again.
func mint(ctx context.Context, c *http.Client, baseURL, email, password, name string) (string, error) {
	base := strings.TrimRight(baseURL, "/")

	if err := post(ctx, c, base+"/api/auth/login", map[string]string{
		"email":    email,
		"password": password,
	}, nil); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusUnauthorized {
			return "", fmt.Errorf("login failed: check the email and password for %s", base)
		}
		return "", fmt.Errorf("log in to %s: %w", base, err)
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := post(ctx, c, base+"/api/tokens", map[string]string{"name": name}, &out); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			return "", fmt.Errorf("%s has no POST /api/tokens: the app must call authSvc.RegisterTokens", base)
		}
		return "", fmt.Errorf("create token: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("create token: the app returned no token")
	}
	return out.Token, nil
}

// post sends a JSON body and, when out is non-nil, decodes a JSON response.
// Non-2xx responses become a *statusError so callers can special-case a status.
func post(ctx context.Context, c *http.Client, url string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBodyBytes))
		return &statusError{code: res.StatusCode, body: strings.TrimSpace(string(snippet))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("HTTP %d", e.code)
	}
	return fmt.Sprintf("HTTP %d: %s", e.code, e.body)
}

// readPassword resolves the account password: $APP_PASSWORD if set, else stdin.
// Echo is disabled when stdin is a terminal, so the password doesn't end up in
// scrollback; a piped password (echo $PW | token ...) reads as one line.
func readPassword(email string) (string, error) {
	if pw := os.Getenv("APP_PASSWORD"); pw != "" {
		return pw, nil
	}

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprintf(os.Stderr, "Password for %s: ", email)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return string(b), nil
	}

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("no password on stdin (set APP_PASSWORD, or run this from a terminal)")
	}
	return line, nil
}

// writeConfig writes ~/.config/<name>.json, the file apiclient.FromConfig reads.
// It returns the path so the caller can report it.
func writeConfig(name, appURL, token string) (string, error) {
	path, err := apiclient.ConfigPath(name)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	// Nothing else creates ~/.config, and this may be the first tool to want it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}

	body, err := json.MarshalIndent(apiclient.FileConfig{AppURL: appURL, Token: token}, "", "  ")
	if err != nil {
		return "", err
	}
	body = append(body, '\n')

	// Temp file (0600) + rename rather than os.WriteFile: WriteFile does not
	// re-apply the mode to a file that already exists, so refreshing a config
	// the user created at 0644 would quietly leave a live token world-readable.
	f, err := os.CreateTemp(dir, "."+name+".json.*")
	if err != nil {
		return "", fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename below succeeds
	if _, err := f.Write(body); err != nil {
		f.Close()
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
