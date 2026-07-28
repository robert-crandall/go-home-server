package mcp

import (
	"strings"
	"testing"
)

func TestAppNameIsABareName(t *testing.T) {
	got := AppName()
	if got == "" {
		t.Fatal("AppName() is empty")
	}
	if strings.ContainsAny(got, `/\`) {
		t.Errorf("AppName() = %q, want a bare name usable as a file name", got)
	}
}

func TestIsMajorVersionSuffix(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"v2", true},
		{"v12", true},
		{"v0", false},
		{"v1", false},
		{"my-app", false},
		{"v", false},
		{"v2x", false},
		{"", false},
	} {
		if got := isMajorVersionSuffix(tc.in); got != tc.want {
			t.Errorf("isMajorVersionSuffix(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
