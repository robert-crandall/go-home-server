// Package templatecheck_test guards invariants of the reference app in
// template/ that only show up when you actually run its tooling.
//
// It lives in the foundation module rather than in template/ on purpose:
// scripts/new-app.sh rsyncs all of template/ into every generated app, and
// these are checks on the template itself, not on the apps made from it.
package templatecheck_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// templateDir returns an absolute path to template/. Go runs tests with the
// working directory set to the package directory, so everything here is
// anchored off this file rather than off a relative path.
func templateDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "template")
}

// readMakefile reads template/Makefile and checks the target under test exists.
//
// Reading it from the test process is what makes it a Go test-cache input: the
// cache only tracks files the test binary itself opens, so a `make` subprocess
// is invisible to it and a later Makefile-only regression would otherwise be
// served a cached PASS. CI runs a plain `go test ./...`, so there is no
// -count=1 to fall back on.
func readMakefile(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		t.Fatalf("read template/Makefile: %v", err)
	}
	if !strings.Contains(string(b), "\nmcp-install:") {
		t.Fatal("template/Makefile has no mcp-install target")
	}
}

func runMake(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("make", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestMCPInstallRelativeBinLandsAtPrintedPath is the regression test for #20.
//
// mcp-install's mkdir and echo run from the app root while its `go build -o`
// runs after `cd server`, so a relative MCP_BIN used to resolve against two
// different directories: the binary landed in server/ while the printed path
// pointed at an empty directory in the app root. Nothing failed - `go build -o`
// creates its own parent - and the READMEs tell you to paste that printed path
// into a desktop MCP client, which execs it directly.
func TestMCPInstallRelativeBinLandsAtPrintedPath(t *testing.T) {
	dir := templateDir(t)
	readMakefile(t, dir)

	const relDir = "tmp-mcpinstall-test"
	const relBin = relDir + "/app-mcp"

	// The whole point is that there are two candidate locations, so clean both
	// up front (a crashed earlier run must not produce a false pass) and again
	// on the way out.
	want := filepath.Join(dir, relBin)
	stale := filepath.Join(dir, "server", relDir)
	clean := func() {
		os.RemoveAll(filepath.Join(dir, relDir))
		os.RemoveAll(stale)
	}
	clean()
	t.Cleanup(clean)

	out := runMake(t, dir, "mcp-install", "MCP_BIN="+relBin)

	fi, err := os.Stat(want)
	if err != nil {
		t.Fatalf("binary not at the app-root-relative path %s: %v\nmake output:\n%s", want, err, out)
	}
	if fi.Mode()&0o111 == 0 {
		t.Errorf("%s is not executable (mode %v)", want, fi.Mode())
	}
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("binary was also built under server/ (%s); MCP_BIN resolved against two directories", stale)
	}

	if got := installedPath(t, out); got != want {
		t.Errorf("mcp-install printed %q, want the absolute path %q", got, want)
	}
}

// TestMCPInstallDefaultBinIsUnchanged pins the default install location, which
// is already absolute and so must survive the abspath resolution untouched.
// This one is a dry run: it only needs the recipe make would have executed.
func TestMCPInstallDefaultBinIsUnchanged(t *testing.T) {
	dir := templateDir(t)
	readMakefile(t, dir)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	// template/server's module path is github.com/robert-crandall/example-app,
	// which is what new-app.sh rewrites when it scaffolds a real app.
	want := filepath.Join(home, "bin", "example-app-mcp")

	out := runMake(t, dir, "-n", "mcp-install")

	if !strings.Contains(out, `-o "`+want+`"`) {
		t.Errorf("dry run does not build to %q\n%s", want, out)
	}
	if got := installedPath(t, out); got != want {
		t.Errorf("mcp-install printed %q, want %q", got, want)
	}
}

// installedPath pulls the path out of mcp-install's
// `installed <path> (config: ...)` line. Under `make -n` the echo is printed as
// the command it would have run, so the line carries an `echo` prefix; the path
// is unambiguous either way because it is bounded by "installed " and
// " (config:".
func installedPath(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		_, rest, ok := strings.Cut(line, "installed ")
		if !ok {
			continue
		}
		path, _, ok := strings.Cut(rest, " (config:")
		if !ok {
			continue
		}
		return path
	}
	t.Fatalf("no `installed ... (config: ...)` line in make output:\n%s", out)
	return ""
}
