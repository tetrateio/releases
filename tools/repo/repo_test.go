package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testFingerprint = "4B08B03E4B1664D681ABAB2C05DD5B6BCB7C4791"

func TestReadRPM(t *testing.T) {
	info, err := readRPM("testdata/tetrate-v1.1.0-rc.1-linux-arm64.rpm")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "tetrate" || info.Arch != "aarch64" || info.Epoch != "0" || info.Version != "1.1.0~rc.1" || info.Release != "1" {
		t.Errorf("name, arch and version: %+v", info)
	}
	if info.License != "Proprietary" || info.Vendor != "Tetrate" || info.InstalledSize == 0 || info.BuildTime != 1762560000 {
		t.Errorf("fields: %+v", info)
	}
	if info.HeaderStart <= 96 || info.HeaderEnd <= info.HeaderStart {
		t.Errorf("header range %d %d", info.HeaderStart, info.HeaderEnd)
	}
	var paths []string
	for _, f := range info.Files {
		paths = append(paths, f.Path)
	}
	if !strings.Contains(strings.Join(paths, "\n"), "/usr/bin/tetrate\n") {
		t.Errorf("files %v", paths)
	}
	found := false
	for _, d := range info.Provides {
		if d.Name == "tetrate" && d.Flags == "EQ" && d.Version == "1.1.0~rc.1" && d.Release == "1" {
			found = true
		}
	}
	if !found {
		t.Errorf("provides %+v", info.Provides)
	}
	for _, d := range info.Requires {
		if strings.HasPrefix(d.Name, "rpmlib(") {
			t.Errorf("requires lists %s", d.Name)
		}
	}
	if _, err := readRPM("testdata/tetrate-v1.0.0-linux-amd64.deb"); err == nil {
		t.Error("a deb read as an rpm")
	}
}

func TestDebControl(t *testing.T) {
	c, err := debControl("testdata/tetrate-v1.0.0-linux-arm64.deb")
	if err != nil {
		t.Fatal(err)
	}
	if controlField(c, "Package") != "tetrate" || controlField(c, "Version") != "1.0.0-1" || controlField(c, "Architecture") != "arm64" {
		t.Errorf("control:\n%s", c)
	}
	if _, err := debControl("testdata/tetrate-v1.0.0-linux-arm64.rpm"); err == nil {
		t.Error("an rpm read as a deb")
	}
}

// fixture writes a checkout with the given versions of tetrate, each with
// the four packages from testdata, and a cache that holds the packages.
func fixture(t *testing.T, versions ...string) (dir, cache string) {
	t.Helper()
	dir, cache = t.TempDir(), t.TempDir()
	products := `{"schemaVersion":1,"packageRepository":{"key":"keys/k.asc","fingerprint":"` + testFingerprint + `"},
"products":{"tetrate":{"packages":{"platforms":["linux-amd64","linux-arm64"],"formats":["deb","rpm"]}}}}`
	write(t, filepath.Join(dir, "products.json"), []byte(products))
	type ver struct {
		Version     string    `json:"version"`
		Prerelease  bool      `json:"prerelease"`
		PublishedAt time.Time `json:"publishedAt"`
		Manifest    string    `json:"manifest"`
	}
	var idx struct {
		Versions []ver `json:"versions"`
	}
	for i, v := range versions {
		published := time.Date(2026, 11, 1+i, 0, 0, 0, 0, time.UTC)
		idx.Versions = append(idx.Versions, ver{v, strings.Contains(v, "-"), published, v + ".json"})
		var pkgs []map[string]any
		for _, pl := range []string{"linux-amd64", "linux-arm64"} {
			for _, f := range []string{"deb", "rpm"} {
				name := fmt.Sprintf("tetrate-%s-%s.%s", v, pl, f)
				b, err := os.ReadFile(filepath.Join("testdata", name))
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256Hex(b)
				write(t, filepath.Join(cache, sum), b)
				pkgs = append(pkgs, map[string]any{
					"name": name, "platform": pl, "format": f, "sha256": sum, "size": len(b),
					"url": releaseDownloads + "tetrate/" + v + "/" + name,
				})
			}
		}
		m, _ := json.Marshal(map[string]any{"product": "tetrate", "version": v, "packages": pkgs})
		write(t, filepath.Join(dir, "tetrate", v+".json"), m)
	}
	b, _ := json.Marshal(idx)
	write(t, filepath.Join(dir, "tetrate", "index.json"), b)
	return dir, cache
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildFixture(t *testing.T, dir, cache string) (map[string][]byte, []pkgFile) {
	t.Helper()
	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	pkgs, date, err := selectPackages(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range pkgs {
		if err := fetch(&pkgs[i], cache); err != nil {
			t.Fatal(err)
		}
	}
	files, err := build(pkgs, cfg, date, releaseDownloads)
	if err != nil {
		t.Fatal(err)
	}
	return files, pkgs
}

func TestSelectPackages(t *testing.T) {
	// Only prereleases: all of them.
	dir, cache := fixture(t, "v1.1.0-rc.1")
	_, pkgs := buildFixture(t, dir, cache)
	if len(pkgs) != 4 {
		t.Fatalf("%d packages, want 4", len(pkgs))
	}
	// A stable release: only stable releases.
	dir, cache = fixture(t, "v1.0.0", "v1.1.0-rc.1")
	_, pkgs = buildFixture(t, dir, cache)
	if len(pkgs) != 4 {
		t.Fatalf("%d packages, want 4", len(pkgs))
	}
	for _, pk := range pkgs {
		if pk.Version != "v1.0.0" {
			t.Errorf("%s is listed", pk.Name)
		}
	}
}

func TestBuild(t *testing.T) {
	dir, cache := fixture(t, "v1.1.0-rc.1")
	files, _ := buildFixture(t, dir, cache)
	again, _ := buildFixture(t, dir, cache)
	for p, b := range files {
		if !bytes.Equal(b, again[p]) {
			t.Errorf("%s differs between two builds", p)
		}
	}

	release := string(files["apt/dists/stable/Release"])
	for _, want := range []string{"Suite: stable\n", "Architectures: amd64 arm64\n", "Components: main\n", "Acquire-By-Hash: yes\n", "Date: Sun, 01 Nov 2026 00:00:00 UTC\n"} {
		if !strings.Contains(release, want) {
			t.Errorf("Release has no %q:\n%s", want, release)
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, name := range []string{"Packages", "Packages.gz"} {
			p := "main/binary-" + arch + "/" + name
			b := files["apt/dists/stable/"+p]
			sum := sha256Hex(b)
			if !strings.Contains(release, fmt.Sprintf(" %s %d %s\n", sum, len(b), p)) {
				t.Errorf("Release does not list %s", p)
			}
			if !bytes.Equal(files["apt/dists/stable/main/binary-"+arch+"/by-hash/SHA256/"+sum], b) {
				t.Errorf("no by-hash copy of %s", p)
			}
		}
	}
	packages := string(files["apt/dists/stable/main/binary-arm64/Packages"])
	for _, want := range []string{
		"Package: tetrate\n", "Version: 1.1.0~rc.1-1\n", "Architecture: arm64\n",
		"Filename: pool/tetrate/v1.1.0-rc.1/tetrate-v1.1.0-rc.1-linux-arm64.deb\n",
	} {
		if !strings.Contains(packages, want) {
			t.Errorf("Packages has no %q:\n%s", want, packages)
		}
	}

	repomd := string(files["rpm/aarch64/repodata/repomd.xml"])
	var primary []byte
	for p, b := range files {
		if strings.HasPrefix(p, "rpm/aarch64/repodata/") && strings.HasSuffix(p, "-primary.xml.gz") {
			if !strings.Contains(repomd, sha256Hex(b)) || !strings.Contains(p, sha256Hex(b)) {
				t.Errorf("%s is not named by its digest in repomd.xml", p)
			}
			zr, err := gzip.NewReader(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			primary, _ = io.ReadAll(zr)
		}
	}
	for _, want := range []string{
		`packages="1"`, `<arch>aarch64</arch>`, `ver="1.1.0~rc.1" rel="1"`,
		`<location xml:base="` + releaseDownloads + `tetrate/v1.1.0-rc.1/" href="tetrate-v1.1.0-rc.1-linux-arm64.rpm"/>`,
		`<file>/usr/bin/tetrate</file>`,
	} {
		if !strings.Contains(string(primary), want) {
			t.Errorf("primary.xml has no %q:\n%s", want, primary)
		}
	}
}

func TestBuildRefusesWrongDigest(t *testing.T) {
	dir, cache := fixture(t, "v1.0.0")
	cfg, _ := loadConfig(dir)
	pkgs, _, err := selectPackages(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pk := pkgs[0]
	pk.SHA256 = strings.Repeat("0", 64)
	pk.URL = "http://127.0.0.1:1/none"
	if err := fetch(&pk, cache); err == nil {
		t.Error("a package with an unknown digest was accepted")
	}
}

func TestCheckSigner(t *testing.T) {
	good := "[GNUPG:] NEWSIG\n[GNUPG:] VALIDSIG 619A8C6E870F7BA1BE3F174FBECC436D664A70F6 2026-11-08 1 0 4 0 1 10 00 " + testFingerprint + "\n"
	if err := checkSigner([]byte(good), testFingerprint); err != nil {
		t.Error(err)
	}
	if checkSigner([]byte(good), strings.Repeat("A", 40)) == nil {
		t.Error("another key accepted")
	}
	if checkSigner([]byte("[GNUPG:] BADSIG x\n"), testFingerprint) == nil {
		t.Error("no valid signature accepted")
	}
}

// TestSignAndCheck signs with a throwaway key, checks the tree, and finds a
// changed file.
func TestSignAndCheck(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg is not installed")
	}
	// A short path, because gpg-agent puts its socket in the home.
	home, err := os.MkdirTemp("/tmp", "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run()
		_ = os.RemoveAll(home)
	})
	t.Setenv("GNUPGHOME", home)
	gpg := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command("gpg", append([]string{"--batch", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...).Output()
		if err != nil {
			t.Fatalf("gpg %v: %v", args, err)
		}
		return out
	}
	gpg("--quick-gen-key", "Test <test@example.com>", "ed25519", "cert", "1d")
	var fpr string
	for line := range strings.SplitSeq(string(gpg("--list-keys", "--with-colons")), "\n") {
		if f := strings.Split(line, ":"); f[0] == "fpr" && fpr == "" {
			fpr = f[9]
		}
	}
	gpg("--quick-add-key", fpr, "ed25519", "sign", "1d")

	dir, cache := fixture(t, "v1.0.0")
	write(t, filepath.Join(dir, "keys/k.asc"), gpg("--armor", "--export", fpr))
	products := strings.ReplaceAll(string(readOrFail(t, filepath.Join(dir, "products.json"))), testFingerprint, fpr)
	write(t, filepath.Join(dir, "products.json"), []byte(products))

	files, _ := buildFixture(t, dir, cache)
	key := filepath.Join(dir, "keys/k.asc")
	n, err := writeTree(dir, files, key, fpr)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("nothing written")
	}
	for _, p := range []string{"apt/dists/stable/InRelease", "apt/dists/stable/Release.gpg", "rpm/x86_64/repodata/repomd.xml.asc"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Error(err)
		}
	}
	if err := checkTree(dir, files, key, fpr); err != nil {
		t.Fatal(err)
	}
	if n, err := writeTree(dir, files, key, fpr); err != nil || n != 0 {
		t.Fatalf("a second run changed %d files: %v", n, err)
	}
	if err := checkTree(dir, files, key, strings.Repeat("A", 40)); err == nil {
		t.Error("a check with another fingerprint passed")
	}
	write(t, filepath.Join(dir, "apt/dists/stable/Release"), []byte("Origin: Other\n"))
	if err := checkTree(dir, files, key, fpr); err == nil {
		t.Error("a changed Release passed the check")
	}
}

func readOrFail(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
