package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tetrate(t *testing.T) Product {
	t.Helper()
	ps, err := loadProducts("../../products.json")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := ps.Products["tetrate"]
	if !ok {
		t.Fatal("products.json has no tetrate")
	}
	return p
}

func TestProductNames(t *testing.T) {
	p := tetrate(t)
	if got := p.AssetName("v0.1.0-alpha.3", "darwin-arm64"); got != "tetrate-v0.1.0-alpha.3-darwin-arm64" {
		t.Errorf("asset %q", got)
	}
	if got := p.BundleName("checksums.txt"); got != "checksums.txt.sigstore.json" {
		t.Errorf("bundle %q", got)
	}
	if got := p.Identity("v0.1.0-alpha.3"); got != "https://github.com/tetrateio/cli/.github/workflows/release.yml@refs/tags/v0.1.0-alpha.3" {
		t.Errorf("identity %q", got)
	}
}

func TestCheckTag(t *testing.T) {
	p := tetrate(t)
	for _, ok := range []string{"v0.1.0", "v0.1.0-alpha.3", "v1.2.3-rc.1"} {
		if err := p.CheckTag(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.1.0", "v0.1", "tetrate/v0.1.0", "v0.1.0-alpha.3 ", "v0.1.0/../x", "programs/llm/v0.1.0"} {
		if p.CheckTag(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLoadProductsRefusesUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "products.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"products":{},"extra":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProducts(path); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestIndexAdd(t *testing.T) {
	at := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	idx := Index{SchemaVersion: 1, Product: "tetrate"}
	add := func(v string, pre bool) {
		t.Helper()
		if err := idx.Add(IndexVersion{Version: v, Prerelease: pre, PublishedAt: at, Manifest: v + ".json"}); err != nil {
			t.Fatal(err)
		}
	}
	add("v0.1.0-alpha.3", true)
	if idx.Latest.Stable != nil || idx.Latest.Prerelease == nil || *idx.Latest.Prerelease != "v0.1.0-alpha.3" {
		t.Fatalf("latest %+v", idx.Latest)
	}
	add("v0.1.0-alpha.10", true) // semantic order, not string order
	add("v0.1.0", false)
	add("v0.2.0-beta.1", true)
	var order []string
	for _, v := range idx.Versions {
		order = append(order, v.Version)
	}
	if got := strings.Join(order, " "); got != "v0.2.0-beta.1 v0.1.0 v0.1.0-alpha.10 v0.1.0-alpha.3" {
		t.Errorf("order %s", got)
	}
	if *idx.Latest.Stable != "v0.1.0" || *idx.Latest.Prerelease != "v0.2.0-beta.1" {
		t.Errorf("latest stable %s prerelease %s", *idx.Latest.Stable, *idx.Latest.Prerelease)
	}
	// The same entry again is a no-op; other content is refused.
	add("v0.1.0", false)
	if len(idx.Versions) != 4 {
		t.Errorf("%d versions after a repeated add", len(idx.Versions))
	}
	if err := idx.Add(IndexVersion{Version: "v0.1.0", Prerelease: true, Manifest: "v0.1.0.json"}); err == nil {
		t.Error("changed entry accepted")
	}
}

func TestLoadIndex(t *testing.T) {
	dir := t.TempDir()
	idx, err := loadIndex(indexPath(dir, "tetrate"), "tetrate")
	if err != nil || len(idx.Versions) != 0 || idx.SchemaVersion != 1 {
		t.Fatalf("missing index: %+v %v", idx, err)
	}
	if err := writeJSON(indexPath(dir, "tetrate"), Index{SchemaVersion: 1, Product: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIndex(indexPath(dir, "tetrate"), "tetrate"); err == nil {
		t.Error("index of another product accepted")
	}
}

func TestReadChecksums(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checksums.txt")
	if err := os.WriteFile(path, []byte("aaa  tetrate-v1-linux-amd64\nbbb *tetrate-v1-darwin-arm64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sums, err := readChecksums(path)
	if err != nil || sums["tetrate-v1-linux-amd64"] != "aaa" || sums["tetrate-v1-darwin-arm64"] != "bbb" {
		t.Fatalf("%v %v", sums, err)
	}
	if err := os.WriteFile(path, []byte("only-one-field\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readChecksums(path); err == nil {
		t.Error("malformed line accepted")
	}
}

func TestBuildManifest(t *testing.T) {
	p := tetrate(t)
	work := t.TempDir()
	for _, pl := range p.Platforms {
		if err := os.WriteFile(filepath.Join(work, p.AssetName("v0.1.0-alpha.3", pl)), []byte(pl), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC)
	src := Release{Prerelease: true, PublishedAt: time.Date(2026, 11, 6, 6, 40, 0, 0, time.UTC)}
	m, err := buildManifest(p, "tetrate", "tetrateio/releases", "v0.1.0-alpha.3", src, "955bd3f", work, now)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Prerelease || m.Source.Repo != "tetrateio/cli" || m.Source.Commit != "955bd3f" || !m.PublishedAt.Equal(now) {
		t.Errorf("manifest %+v", m)
	}
	if len(m.Assets) != 3 {
		t.Fatalf("%d assets", len(m.Assets))
	}
	a := m.Assets[0]
	if a.Platform != "darwin-arm64" || a.Size != int64(len("darwin-arm64")) || len(a.SHA256) != 64 {
		t.Errorf("asset %+v", a)
	}
	wantURL := "https://github.com/tetrateio/releases/releases/download/tetrate/v0.1.0-alpha.3/tetrate-v0.1.0-alpha.3-darwin-arm64"
	if a.URL != wantURL || a.Bundle != wantURL+".sigstore.json" {
		t.Errorf("urls %s %s", a.URL, a.Bundle)
	}
	if m.Release != "https://github.com/tetrateio/releases/releases/tag/tetrate/v0.1.0-alpha.3" {
		t.Errorf("release %s", m.Release)
	}
	if m.Signer.Identity != p.Identity("v0.1.0-alpha.3") {
		t.Errorf("signer %s", m.Signer.Identity)
	}
}

func TestReleaseBody(t *testing.T) {
	b := releaseBody("tetrate", "v0.1.0-alpha.3", "tetrateio/cli", "955bd3f", "# notes\n")
	for _, want := range []string{"promoted from release v0.1.0-alpha.3 of tetrateio/cli (commit 955bd3f)", "tetrate/v0.1.0-alpha.3.json", "# notes"} {
		if !strings.Contains(b, want) {
			t.Errorf("body lacks %q:\n%s", want, b)
		}
	}
}
