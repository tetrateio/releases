// Command repo writes the apt and dnf repositories of the Linux packages
// that this repository publishes, and signs their metadata:
//
//	go run ./tools/repo            write apt/ and rpm/, sign what changed
//	go run ./tools/repo -commit    the same, then commit and push
//	go run ./tools/repo -check     rebuild in memory, compare with the
//	                               checkout and verify every signature
//
// The packages stay in the GitHub releases. apt/ and rpm/ hold only the
// indexes: one apt suite, stable, with the component main, and one dnf
// repository per architecture. get.tetrate.io serves both trees and
// redirects the apt pool to the release downloads. dnf reads each package
// from its release download through xml:base.
//
// A repository lists every stable release of a product that has packages.
// While a product has no stable release, it lists every prerelease
// instead, the same rule as the installer.
//
// Signing runs gpg with the key in products.json. It needs the signing
// subkey only. Files that did not change keep their signatures, so running
// the tool twice changes nothing.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// releaseDownloads is the prefix of every package URL. The apt pool path of
// a package is its URL without this prefix.
const releaseDownloads = "https://github.com/tetrateio/releases/releases/download/"

func main() {
	dir := flag.String("dir", ".", "the checkout of this repository")
	cache := flag.String("cache", "", "the directory that keeps downloaded packages; a directory under the user cache when empty")
	downloads := flag.String("downloads", releaseDownloads, "the URL prefix of every package; tests use a local server")
	check := flag.Bool("check", false, "rebuild in memory, compare with the checkout and verify the signatures; change nothing")
	commit := flag.Bool("commit", false, "commit apt/ and rpm/ with a signed commit and push")
	flag.Parse()
	if flag.NArg() != 0 || (*check && *commit) {
		fmt.Fprintln(os.Stderr, "usage: repo [-check | -commit] [-dir DIR] [-cache DIR]")
		os.Exit(2)
	}
	if *cache == "" {
		c, err := os.UserCacheDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "repo:", err)
			os.Exit(1)
		}
		*cache = filepath.Join(c, "tetrate-releases", "packages")
	}
	if err := run(*dir, *cache, *downloads, *check, *commit); err != nil {
		fmt.Fprintln(os.Stderr, "repo:", err)
		os.Exit(1)
	}
}

func run(dir, cache, downloads string, check, commit bool) error {
	cfg, err := loadConfig(dir)
	if err != nil {
		return err
	}
	if commit {
		if err := checkCheckout(dir); err != nil {
			return err
		}
	}
	pkgs, date, err := selectPackages(dir, cfg)
	if err != nil {
		return err
	}
	for i := range pkgs {
		if err := fetch(&pkgs[i], cache); err != nil {
			return err
		}
	}
	files, err := build(pkgs, cfg, date, downloads)
	if err != nil {
		return err
	}
	key := filepath.Join(dir, cfg.Repository.Key)
	if check {
		return checkTree(dir, files, key, cfg.Repository.Fingerprint)
	}
	changed, err := writeTree(dir, files, key, cfg.Repository.Fingerprint)
	if err != nil {
		return err
	}
	fmt.Printf("%d packages, %d files changed\n", len(pkgs), changed)
	if !commit || changed == 0 {
		return nil
	}
	for _, args := range [][]string{
		{"add", "--", "apt", "rpm"},
		{"commit", "-S", "-m", "Update the package repositories"},
		{"push", "origin", "HEAD:main"},
	} {
		if _, err := git(dir, args...); err != nil {
			return err
		}
	}
	fmt.Println("Pushed the package repositories")
	return nil
}

// config is the part of products.json that this tool reads.
type config struct {
	Repository struct {
		Key         string `json:"key"`
		Fingerprint string `json:"fingerprint"`
	} `json:"packageRepository"`
	Products map[string]struct {
		Packages *struct {
			Platforms []string `json:"platforms"`
			Formats   []string `json:"formats"`
		} `json:"packages"`
	} `json:"products"`
}

func loadConfig(dir string) (config, error) {
	var cfg config
	b, err := os.ReadFile(filepath.Join(dir, "products.json"))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("products.json: %w", err)
	}
	if cfg.Repository.Key == "" || len(cfg.Repository.Fingerprint) != 40 {
		return cfg, errors.New("products.json: packageRepository needs key and a 40 digit fingerprint")
	}
	return cfg, nil
}

// arches lists the architectures of the configured packages, in deb naming.
func (cfg config) arches() []string {
	seen := map[string]bool{}
	for _, p := range cfg.Products {
		if p.Packages == nil {
			continue
		}
		for _, pl := range p.Packages.Platforms {
			seen[strings.TrimPrefix(pl, "linux-")] = true
		}
	}
	var out []string
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// pkgFile is one package of a promoted release.
type pkgFile struct {
	Product  string
	Version  string
	Name     string
	Platform string
	Format   string
	SHA256   string
	Size     int64
	URL      string
	// Path is the local copy, set by fetch.
	Path string
}

// selectPackages reads the indexes and manifests and returns the packages
// that the repositories list, oldest version first, and the time of the
// newest release of a product with packages.
func selectPackages(dir string, cfg config) ([]pkgFile, time.Time, error) {
	var out []pkgFile
	var date time.Time
	var names []string
	for name, p := range cfg.Products {
		if p.Packages != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var idx struct {
			Versions []struct {
				Version     string    `json:"version"`
				Prerelease  bool      `json:"prerelease"`
				PublishedAt time.Time `json:"publishedAt"`
				Manifest    string    `json:"manifest"`
			} `json:"versions"`
		}
		b, err := os.ReadFile(filepath.Join(dir, name, "index.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, date, err
		}
		if err := json.Unmarshal(b, &idx); err != nil {
			return nil, date, fmt.Errorf("%s/index.json: %w", name, err)
		}
		stable := false
		for _, v := range idx.Versions {
			stable = stable || !v.Prerelease
			if v.PublishedAt.After(date) {
				date = v.PublishedAt
			}
		}
		var versions []string
		manifests := map[string]string{}
		for _, v := range idx.Versions {
			if stable && v.Prerelease {
				continue
			}
			versions = append(versions, v.Version)
			manifests[v.Version] = v.Manifest
		}
		sort.Slice(versions, func(i, j int) bool { return semver.Compare(versions[i], versions[j]) < 0 })
		for _, v := range versions {
			var m struct {
				Product  string    `json:"product"`
				Version  string    `json:"version"`
				Packages []pkgFile `json:"packages"`
			}
			b, err := os.ReadFile(filepath.Join(dir, name, filepath.Base(manifests[v])))
			if err != nil {
				return nil, date, err
			}
			if err := json.Unmarshal(b, &m); err != nil {
				return nil, date, fmt.Errorf("%s/%s: %w", name, manifests[v], err)
			}
			if m.Product != name || m.Version != v {
				return nil, date, fmt.Errorf("%s/%s names %s %s", name, manifests[v], m.Product, m.Version)
			}
			for _, pk := range m.Packages {
				pk.Product, pk.Version = name, v
				out = append(out, pk)
			}
		}
	}
	if date.IsZero() {
		date = time.Unix(0, 0)
	}
	return out, date.UTC().Truncate(time.Second), nil
}

// fetch puts the package into the cache, named by its digest, and checks
// its sha256 and size against the manifest.
func fetch(pk *pkgFile, cache string) error {
	if len(pk.SHA256) != 64 {
		return fmt.Errorf("%s: the manifest has no sha256", pk.Name)
	}
	path := filepath.Join(cache, pk.SHA256)
	if sum, size, err := digest(path); err == nil && sum == pk.SHA256 && size == pk.Size {
		pk.Path = path
		return nil
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	fmt.Printf("Downloading %s\n", pk.URL)
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(pk.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", pk.URL, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(cache, "download-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	sum, size, err := digest(tmp.Name())
	if err != nil {
		return err
	}
	if sum != pk.SHA256 || size != pk.Size {
		return fmt.Errorf("%s: sha256 %s and size %d, the manifest says %s and %d", pk.URL, sum, size, pk.SHA256, pk.Size)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	pk.Path = path
	return nil
}

func digest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// build returns every unsigned file of both trees, keyed by its path in
// the checkout.
func build(pkgs []pkgFile, cfg config, date time.Time, downloads string) (map[string][]byte, error) {
	var debs, rpms []pkgFile
	for _, pk := range pkgs {
		switch pk.Format {
		case "deb":
			debs = append(debs, pk)
		case "rpm":
			rpms = append(rpms, pk)
		default:
			return nil, fmt.Errorf("%s: unknown format %q", pk.Name, pk.Format)
		}
	}
	files := map[string][]byte{}
	apt, err := buildApt(debs, cfg.arches(), date, downloads)
	if err != nil {
		return nil, err
	}
	for k, v := range apt {
		files["apt/"+k] = v
	}
	dnf, err := buildDnf(rpms, cfg.arches(), date)
	if err != nil {
		return nil, err
	}
	for k, v := range dnf {
		files["rpm/"+k] = v
	}
	return files, nil
}

// signatures returns the signature files of a file of the tree: InRelease
// and Release.gpg for an apt Release, repomd.xml.asc for a dnf repomd.xml.
func signatures(path string) (clear, detached string) {
	switch filepath.Base(path) {
	case "Release":
		return filepath.Join(filepath.Dir(path), "InRelease"), path + ".gpg"
	case "repomd.xml":
		return "", path + ".asc"
	}
	return "", ""
}

// writeTree writes the files that differ and signs every signed file that
// changed or whose signature does not verify. Files that are not in the
// tree stay, so a client that read an older index still finds its files.
func writeTree(dir string, files map[string][]byte, key, fingerprint string) (int, error) {
	paths := sortedKeys(files)
	changed := 0
	for _, p := range paths {
		full := filepath.Join(dir, p)
		old, err := os.ReadFile(full)
		same := err == nil && bytes.Equal(old, files[p])
		if !same {
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return changed, err
			}
			if err := os.WriteFile(full, files[p], 0o644); err != nil {
				return changed, err
			}
			changed++
		}
		clear, detached := signatures(p)
		if detached == "" {
			continue
		}
		if same && verifySignatures(dir, p, files[p], key, fingerprint) == nil {
			continue
		}
		fmt.Printf("Signing %s\n", p)
		if clear != "" {
			if err := gpgSign(fingerprint, full, filepath.Join(dir, clear), true); err != nil {
				return changed, err
			}
			changed++
		}
		if err := gpgSign(fingerprint, full, filepath.Join(dir, detached), false); err != nil {
			return changed, err
		}
		changed++
		if err := verifySignatures(dir, p, files[p], key, fingerprint); err != nil {
			return changed, fmt.Errorf("the new signature of %s does not verify with %s: %w", p, key, err)
		}
	}
	return changed, nil
}

// checkTree compares the checkout with the rebuilt files and verifies the
// signatures with the public key.
func checkTree(dir string, files map[string][]byte, key, fingerprint string) error {
	var errs []error
	for _, p := range sortedKeys(files) {
		old, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !bytes.Equal(old, files[p]) {
			errs = append(errs, fmt.Errorf("%s differs from the rebuilt file. Run go run ./tools/repo", p))
			continue
		}
		if _, detached := signatures(p); detached != "" {
			if err := verifySignatures(dir, p, files[p], key, fingerprint); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p, err))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	fmt.Printf("Checked %d files and their signatures\n", len(files))
	return nil
}

// verifySignatures checks the signatures of one signed file. InRelease
// must also hold exactly the Release text.
func verifySignatures(dir, path string, content []byte, key, fingerprint string) error {
	clear, detached := signatures(path)
	if err := gpgVerify(key, fingerprint, filepath.Join(dir, detached), filepath.Join(dir, path)); err != nil {
		return err
	}
	if clear == "" {
		return nil
	}
	text, err := gpgVerifyClear(key, fingerprint, filepath.Join(dir, clear))
	if err != nil {
		return err
	}
	if !bytes.Equal(bytes.TrimRight(text, "\n"), bytes.TrimRight(content, "\n")) {
		return fmt.Errorf("%s does not hold the text of %s", clear, path)
	}
	return nil
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkCheckout requires a clean main that equals origin/main.
func checkCheckout(dir string) error {
	if out, err := git(dir, "status", "--porcelain"); err != nil {
		return err
	} else if out != "" {
		return fmt.Errorf("the checkout has changes:\n%s", out)
	}
	if b, err := git(dir, "rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		return err
	} else if b != "main" {
		return fmt.Errorf("the checkout is on %s, not main", b)
	}
	if _, err := git(dir, "fetch", "--quiet", "origin", "main"); err != nil {
		return err
	}
	head, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	remote, err := git(dir, "rev-parse", "origin/main")
	if err != nil {
		return err
	}
	if head != remote {
		return errors.New("main is not origin/main. Pull or push first")
	}
	return nil
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
