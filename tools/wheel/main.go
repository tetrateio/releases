// Command wheel builds the PyPI wheels of a promoted release from the files
// published in this repository:
//
//	go run ./tools/wheel -product tetrate -out out/pypi
//
// Without -version it uses the newest release that the index names. It
// downloads each binary from the manifest URL with no credential, checks its
// sha256 against the manifest and, with -cosign, its sigstore bundle against
// the signer in the manifest.
//
// Each wheel holds one binary and no Python code. The binary is in the
// wheel's scripts directory, so pip and uv install it into bin as it is. The
// Go binaries are static, so one Linux wheel serves glibc and musl hosts.
// There is no source distribution: on another platform pip finds no wheel
// instead of trying to build one.
package main

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const repository = "https://github.com/tetrateio/releases"

// platformTags lists the wheel platform tags of each manifest platform.
var platformTags = map[string][]string{
	"darwin-arm64": {"macosx_11_0_arm64"},
	"linux-amd64":  {"manylinux_2_17_x86_64", "manylinux2014_x86_64", "musllinux_1_2_x86_64"},
	"linux-arm64":  {"manylinux_2_17_aarch64", "manylinux2014_aarch64", "musllinux_1_2_aarch64"},
}

func main() {
	product := flag.String("product", "", "the product, a directory of this repository")
	version := flag.String("version", "", "the version; the newest in the index when empty")
	dir := flag.String("dir", ".", "the checkout of this repository")
	out := flag.String("out", "out/pypi", "the directory for the wheels; it is replaced")
	cosign := flag.Bool("cosign", false, "also verify each binary's sigstore bundle with cosign")
	flag.Parse()
	if *product != "tetrate" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: wheel -product tetrate [-version vX] [-out dir] [-cosign]")
		os.Exit(2)
	}
	if err := run(context.Background(), *dir, *out, *product, *version, *cosign); err != nil {
		fmt.Fprintln(os.Stderr, "wheel:", err)
		os.Exit(1)
	}
}

type asset struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	SHA256   string `json:"sha256"`
	URL      string `json:"url"`
	Bundle   string `json:"bundle"`
}

type manifest struct {
	Product string `json:"product"`
	Version string `json:"version"`
	Signer  struct {
		Issuer   string `json:"issuer"`
		Identity string `json:"identity"`
	} `json:"signer"`
	Source struct {
		Repo string `json:"repo"`
		Tag  string `json:"tag"`
	} `json:"source"`
	Assets []asset `json:"assets"`
}

func run(ctx context.Context, dir, out, product, version string, cosign bool) error {
	if version == "" {
		var idx struct {
			Latest struct {
				Stable     *string `json:"stable"`
				Prerelease *string `json:"prerelease"`
			} `json:"latest"`
		}
		if err := readJSON(filepath.Join(dir, product, "index.json"), &idx); err != nil {
			return err
		}
		switch {
		case idx.Latest.Stable != nil:
			version = *idx.Latest.Stable
		case idx.Latest.Prerelease != nil:
			version = *idx.Latest.Prerelease
		default:
			return errors.New("the index names no release")
		}
	}
	var m manifest
	if err := readJSON(filepath.Join(dir, product, version+".json"), &m); err != nil {
		return fmt.Errorf("%s %s is not promoted: %w", product, version, err)
	}
	pyVersion, err := pep440(m.Version)
	if err != nil {
		return err
	}
	license, err := os.ReadFile(filepath.Join(dir, "LICENSE"))
	if err != nil {
		return err
	}
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "wheel-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	platforms := make([]string, 0, len(platformTags))
	for p := range platformTags {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		var a *asset
		for i := range m.Assets {
			if m.Assets[i].Platform == platform {
				a = &m.Assets[i]
			}
		}
		if a == nil {
			return fmt.Errorf("%s %s has no %s binary", product, version, platform)
		}
		bin := filepath.Join(work, a.Name)
		fmt.Printf("Downloading %s\n", a.Name)
		if err := download(ctx, a.URL, bin); err != nil {
			return err
		}
		sum, err := fileSHA256(bin)
		if err != nil {
			return err
		}
		if sum != a.SHA256 {
			return fmt.Errorf("%s: sha256 %s, want %s from the manifest", a.Name, sum, a.SHA256)
		}
		if cosign {
			if err := verify(ctx, m, a.Bundle, bin, bin+".sigstore.json"); err != nil {
				return err
			}
			fmt.Printf("Verified %s: signed by %s\n", a.Name, m.Signer.Identity)
		}
		body, err := os.ReadFile(bin)
		if err != nil {
			return err
		}
		name, err := writeWheel(out, product, pyVersion, m.Version, platformTags[platform], body, license)
		if err != nil {
			return err
		}
		fmt.Printf("Wrote %s\n", name)
	}
	return nil
}

var semverPre = regexp.MustCompile(`^v(\d+\.\d+\.\d+)(?:-(alpha|beta|rc)\.(\d+))?$`)

// pep440 maps a release tag to a PyPI version: v0.1.0-alpha.3 is 0.1.0a3,
// -beta.N is bN and -rc.N is rcN. Any other prerelease has no PyPI form.
func pep440(tag string) (string, error) {
	m := semverPre.FindStringSubmatch(tag)
	if m == nil {
		return "", fmt.Errorf("version %s has no PyPI form: only vX.Y.Z and -alpha.N, -beta.N or -rc.N map", tag)
	}
	switch m[2] {
	case "":
		return m[1], nil
	case "alpha":
		return m[1] + "a" + m[3], nil
	case "beta":
		return m[1] + "b" + m[3], nil
	}
	return m[1] + "rc" + m[3], nil
}

// zipTime is the timestamp of every wheel entry, so the same inputs give the
// same wheel.
var zipTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// writeWheel writes one wheel and returns its file name.
func writeWheel(out, product, pyVersion, tag string, tags []string, binary, license []byte) (string, error) {
	dist := product + "-" + pyVersion
	name := fmt.Sprintf("%s-py3-none-%s.whl", dist, strings.Join(tags, "."))
	files := []struct {
		path string
		body []byte
		mode os.FileMode
	}{
		{dist + ".data/scripts/" + product, binary, 0o755},
		{dist + ".dist-info/METADATA", []byte(metadata(product, pyVersion, tag)), 0o644},
		{dist + ".dist-info/WHEEL", []byte(wheelFile(tags)), 0o644},
		{dist + ".dist-info/licenses/LICENSE", license, 0o644},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var record strings.Builder
	for _, f := range files {
		if err := addFile(zw, f.path, f.body, f.mode); err != nil {
			return "", err
		}
		h := sha256.Sum256(f.body)
		fmt.Fprintf(&record, "%s,sha256=%s,%d\n", f.path, base64.RawURLEncoding.EncodeToString(h[:]), len(f.body))
	}
	recordPath := dist + ".dist-info/RECORD"
	fmt.Fprintf(&record, "%s,,\n", recordPath)
	if err := addFile(zw, recordPath, []byte(record.String()), 0o644); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return name, os.WriteFile(filepath.Join(out, name), buf.Bytes(), 0o644)
}

// addFile writes one entry with its CRC and sizes in the header. PyPI refuses
// a wheel whose entries end with a data descriptor, which zip.Writer.CreateHeader
// writes, so the entry is compressed first and written with CreateRaw.
func addFile(zw *zip.Writer, path string, body []byte, mode os.FileMode) error {
	var compressed bytes.Buffer
	fw, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		return err
	}
	if _, err := fw.Write(body); err != nil {
		return err
	}
	if err := fw.Close(); err != nil {
		return err
	}
	h := &zip.FileHeader{
		Name:               path,
		Method:             zip.Deflate,
		Modified:           zipTime,
		CRC32:              crc32.ChecksumIEEE(body),
		CompressedSize64:   uint64(compressed.Len()),
		UncompressedSize64: uint64(len(body)),
	}
	h.SetMode(mode)
	w, err := zw.CreateRaw(h)
	if err != nil {
		return err
	}
	_, err = w.Write(compressed.Bytes())
	return err
}

func metadata(product, pyVersion, tag string) string {
	var b strings.Builder
	b.WriteString("Metadata-Version: 2.4\n")
	fmt.Fprintf(&b, "Name: %s\n", product)
	fmt.Fprintf(&b, "Version: %s\n", pyVersion)
	b.WriteString("Summary: tetrate, the command for Tetrate products: install, verify and run them.\n")
	b.WriteString("License-File: LICENSE\n")
	b.WriteString("Classifier: License :: Other/Proprietary License\n")
	b.WriteString("Classifier: Environment :: Console\n")
	b.WriteString("Classifier: Operating System :: MacOS\n")
	b.WriteString("Classifier: Operating System :: POSIX :: Linux\n")
	b.WriteString("Requires-Python: >=3.8\n")
	fmt.Fprintf(&b, "Project-URL: Homepage, %s\n", repository)
	fmt.Fprintf(&b, "Project-URL: Release, %s/releases/tag/%s/%s\n", repository, product, tag)
	b.WriteString("Description-Content-Type: text/markdown\n\n")
	fmt.Fprintf(&b, "# tetrate\n\n`tetrate` %s is the command for Tetrate products. It installs, verifies and runs them.\n\n", tag)
	b.WriteString("```sh\nuvx tetrate install router\nuv tool install tetrate\npipx install tetrate\n```\n\n")
	b.WriteString("Each wheel holds the `tetrate` binary for one platform: macOS on Apple silicon, and Linux on x86_64 or arm64, glibc or musl.\n\n")
	b.WriteString("Router releases are private, so `tetrate install router` needs a GitHub token that can read tetrateio/router, or a logged-in `gh`.\n\n")
	fmt.Fprintf(&b, "The binaries are published in [tetrateio/releases](%s), where each one has a sigstore signature.\n", repository)
	return b.String()
}

func wheelFile(tags []string) string {
	var b strings.Builder
	b.WriteString("Wheel-Version: 1.0\n")
	b.WriteString("Generator: tetrateio/releases tools/wheel\n")
	b.WriteString("Root-Is-Purelib: false\n")
	for _, t := range tags {
		fmt.Fprintf(&b, "Tag: py3-none-%s\n", t)
	}
	return b.String()
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// download fetches a public URL of this repository with no credential.
func download(ctx context.Context, url, path string) error {
	if !strings.HasPrefix(url, repository+"/releases/download/") {
		return fmt.Errorf("refusing a download outside %s: %s", repository, url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verify checks a binary against its bundle and the signer of the manifest.
func verify(ctx context.Context, m manifest, bundleURL, bin, bundlePath string) error {
	if err := download(ctx, bundleURL, bundlePath); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "cosign", "verify-blob",
		"--bundle", bundlePath,
		"--certificate-identity", m.Signer.Identity,
		"--certificate-oidc-issuer", m.Signer.Issuer,
		"--certificate-github-workflow-repository", m.Source.Repo,
		"--certificate-github-workflow-ref", "refs/tags/"+m.Source.Tag,
		"--certificate-github-workflow-trigger", "push",
		bin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the signature of %s does not verify: %s", filepath.Base(bin), strings.TrimSpace(stderr.String()))
	}
	return nil
}
