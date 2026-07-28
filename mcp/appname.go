package mcp

import (
	"path"
	"runtime/debug"
	"strings"
)

// AppName returns the app's short name, derived from the main module path of
// the running binary: github.com/you/my-app -> "my-app". It falls back to "app"
// when build info is unavailable.
//
// It exists so one name drives everything a user has to line up by hand:
//
//	MCP server name      AppName() + "-mcp"          (the initialize handshake)
//	installed binary     $HOME/bin/<AppName()>-mcp   (make mcp-install)
//	config file          ~/.config/<AppName()>.json  (apiclient.FromConfig)
//
// A /vN module suffix is dropped, so github.com/you/my-app/v2 is still "my-app".
func AppName() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "app"
	}
	p := strings.TrimSuffix(bi.Main.Path, "/")
	base := path.Base(p)
	if isMajorVersionSuffix(base) {
		base = path.Base(path.Dir(p))
	}
	if base == "" || base == "." || base == "/" {
		return "app"
	}
	return base
}

// isMajorVersionSuffix reports whether s is a Go module major-version element
// like "v2" (v0 and v1 are never path elements, so they don't count).
func isMajorVersionSuffix(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != "v0" && s != "v1"
}
