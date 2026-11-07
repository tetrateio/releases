package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderFromThePromotedManifest(t *testing.T) {
	b, err := os.ReadFile("../../tetrate/v0.1.0-alpha.3.json")
	if err != nil {
		t.Skip("no promoted manifest in this checkout")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tetrate"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tetrate", "v0.1.0-alpha.3.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	idx := `{"schemaVersion":1,"product":"tetrate","latest":{"stable":null,"prerelease":"v0.1.0-alpha.3"},"versions":[]}`
	if err := os.WriteFile(filepath.Join(dir, "tetrate", "index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	tap := t.TempDir()
	if err := run(dir, tap, "tetrate", "", false); err != nil {
		t.Fatal(err)
	}
	rb, err := os.ReadFile(filepath.Join(tap, "Formula", "tetrate.rb"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(rb)
	for _, want := range []string{
		"class Tetrate < Formula",
		`version "0.1.0-alpha.3"`,
		"license :cannot_represent",
		"depends_on arch: :arm64",
		`url "https://github.com/tetrateio/releases/releases/download/tetrate/v0.1.0-alpha.3/tetrate-v0.1.0-alpha.3-darwin-arm64"`,
		`sha256 "9e23f11f38b392296d80bdcc29dcbe513671913d769d7b3b04de0ee4463161bf"`,
		"on_intel do", "on_arm do",
		`generate_completions_from_executable(bin/"tetrate", "completion")`,
		`shell_output("#{bin}/tetrate version")`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("formula lacks %q:\n%s", want, s)
		}
	}
	if strings.Count(s, "sha256 \"") != 3 {
		t.Errorf("want 3 downloads:\n%s", s)
	}
}

func TestRenderRefuses(t *testing.T) {
	good := manifest{Product: "tetrate", Version: "v1.0.0"}
	for _, p := range []string{"darwin-arm64", "linux-amd64", "linux-arm64"} {
		good.Assets = append(good.Assets, struct {
			Platform string `json:"platform"`
			SHA256   string `json:"sha256"`
			URL      string `json:"url"`
		}{p, strings.Repeat("a", 64), "https://github.com/tetrateio/releases/releases/download/tetrate/v1.0.0/x-" + p})
	}
	if _, err := render(good); err != nil {
		t.Fatal(err)
	}
	missing := good
	missing.Assets = good.Assets[:2]
	if _, err := render(missing); err == nil {
		t.Error("a missing platform was accepted")
	}
	foreign := good
	foreign.Assets = append([]struct {
		Platform string `json:"platform"`
		SHA256   string `json:"sha256"`
		URL      string `json:"url"`
	}{}, good.Assets...)
	foreign.Assets[0].URL = "https://example.com/tetrate"
	if _, err := render(foreign); err == nil {
		t.Error("a URL outside tetrateio/releases was accepted")
	}
}
