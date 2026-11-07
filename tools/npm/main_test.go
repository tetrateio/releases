package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDistTag(t *testing.T) {
	stable := "v1.0.0"
	for _, tc := range []struct {
		pre       bool
		hasStable bool
		want      string
	}{
		{false, true, "latest"},
		{false, false, "latest"},
		{true, false, "latest"}, // a prerelease while no stable release exists
		{true, true, "next"},
	} {
		var idx index
		if tc.hasStable {
			idx.Latest.Stable = &stable
		}
		if got := distTag(manifest{Prerelease: tc.pre}, idx); got != tc.want {
			t.Errorf("prerelease=%v stable=%v: %s, want %s", tc.pre, tc.hasStable, got, tc.want)
		}
	}
}

func TestDownloadRefusesOtherHosts(t *testing.T) {
	if err := download(t.Context(), "https://example.com/tetrate", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("a URL outside tetrateio/releases was accepted")
	}
}

// hostPackage is the platform package npm would install on this host.
func hostPackage(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS + " " + runtime.GOARCH {
	case "darwin arm64":
		return "@tetrate/cli-darwin-arm64"
	case "linux amd64":
		return "@tetrate/cli-linux-x64"
	case "linux arm64":
		return "@tetrate/cli-linux-arm64"
	}
	t.Skip("no tetrate package for this host")
	return ""
}

// launcherProject lays out node_modules as npm installs the main package
// and, when withBinary is true, a fake binary in the host's platform package.
func launcherProject(t *testing.T, withBinary bool) string {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required")
	}
	dir := t.TempDir()
	cli := filepath.Join(dir, "node_modules", "@tetrate", "cli", "bin")
	if err := os.MkdirAll(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cli, "tetrate.js"), []byte(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if withBinary {
		bin := filepath.Join(dir, "node_modules", hostPackage(t), "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		// Mode 0644: npm can lose the executable bit, and the launcher
		// restores it.
		if err := os.WriteFile(filepath.Join(bin, "tetrate"), []byte("#!/bin/sh\necho \"args: $*\"\nexit 7\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(cli, "tetrate.js")
}

func TestLauncherRunsThePlatformBinary(t *testing.T) {
	js := launcherProject(t, true)
	out, err := exec.Command("node", js, "version", "--x", "a b").CombinedOutput()
	var ee *exec.ExitError
	if !strings.Contains(string(out), "args: version --x a b") {
		t.Errorf("output %q", out)
	}
	if err == nil || !asExit(err, &ee) || ee.ExitCode() != 7 {
		t.Errorf("exit %v, want 7", err)
	}
}

func TestLauncherWithoutThePlatformPackage(t *testing.T) {
	js := launcherProject(t, false)
	out, err := exec.Command("node", js, "version").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "is not installed. Install again without --omit=optional") {
		t.Errorf("%v %s", err, out)
	}
}

func asExit(err error, ee **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	*ee = e
	return ok
}
