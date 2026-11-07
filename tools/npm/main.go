// Command npm builds the npm packages of a promoted release from the files
// published in this repository:
//
//	go run ./tools/npm -product tetrate -out out/npm
//
// Without -version it uses the newest release that the index names. It
// downloads each binary from the manifest URL with no credential, checks its
// sha256 against the manifest and, with -cosign, its sigstore bundle against
// the signer in the manifest. It then writes one directory per package and
// out/npm/publish.txt, which lists the directories in publish order and the
// dist-tag to publish them with.
//
// The packages: one per platform, which holds the binary and declares its
// os and cpu, so npm installs only the one that matches the host; the main
// package, which depends on all three as optional dependencies and runs the
// matching binary; and the unscoped wrapper, which depends on the main
// package.
package main

import (
	"bytes"
	"context"
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
	"strings"
	"time"
)

// npmName is the package of each platform of the manifest. npm calls amd64
// x64.
var npmPlatforms = []struct{ Platform, Package, OS, CPU string }{
	{"darwin-arm64", "cli-darwin-arm64", "darwin", "arm64"},
	{"linux-amd64", "cli-linux-x64", "linux", "x64"},
	{"linux-arm64", "cli-linux-arm64", "linux", "arm64"},
}

const (
	scope      = "@tetrate"
	wrapper    = "tetrate"
	repository = "https://github.com/tetrateio/releases"
)

func main() {
	product := flag.String("product", "", "the product, a directory of this repository")
	version := flag.String("version", "", "the version; the newest in the index when empty")
	dir := flag.String("dir", ".", "the checkout of this repository")
	out := flag.String("out", "out/npm", "the directory for the packages; it is replaced")
	cosign := flag.Bool("cosign", false, "also verify each binary's sigstore bundle with cosign")
	flag.Parse()
	if *product != "tetrate" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: npm -product tetrate [-version vX] [-out dir] [-cosign]")
		os.Exit(2)
	}
	if err := run(context.Background(), *dir, *out, *product, *version, *cosign); err != nil {
		fmt.Fprintln(os.Stderr, "npm:", err)
		os.Exit(1)
	}
}

type manifest struct {
	Product    string `json:"product"`
	Version    string `json:"version"`
	Prerelease bool   `json:"prerelease"`
	Signer     struct {
		Issuer   string `json:"issuer"`
		Identity string `json:"identity"`
	} `json:"signer"`
	Source struct {
		Repo string `json:"repo"`
		Tag  string `json:"tag"`
	} `json:"source"`
	Assets []struct {
		Name     string `json:"name"`
		Platform string `json:"platform"`
		SHA256   string `json:"sha256"`
		URL      string `json:"url"`
		Bundle   string `json:"bundle"`
	} `json:"assets"`
}

type index struct {
	Latest struct {
		Stable     *string `json:"stable"`
		Prerelease *string `json:"prerelease"`
	} `json:"latest"`
}

func run(ctx context.Context, dir, out, product, version string, cosign bool) error {
	var idx index
	if err := readJSON(filepath.Join(dir, product, "index.json"), &idx); err != nil {
		return err
	}
	if version == "" {
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
	npmVersion := strings.TrimPrefix(m.Version, "v")
	tag := distTag(m, idx)

	if err := os.RemoveAll(out); err != nil {
		return err
	}
	license, err := os.ReadFile(filepath.Join(dir, "LICENSE"))
	if err != nil {
		return err
	}

	var order []string
	optional := map[string]string{}
	for _, np := range npmPlatforms {
		var asset *struct {
			Name     string `json:"name"`
			Platform string `json:"platform"`
			SHA256   string `json:"sha256"`
			URL      string `json:"url"`
			Bundle   string `json:"bundle"`
		}
		for i := range m.Assets {
			if m.Assets[i].Platform == np.Platform {
				asset = &m.Assets[i]
			}
		}
		if asset == nil {
			return fmt.Errorf("%s %s has no %s binary", product, version, np.Platform)
		}
		pkgDir := filepath.Join(out, np.Package)
		bin := filepath.Join(pkgDir, "bin", "tetrate")
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return err
		}
		fmt.Printf("Downloading %s\n", asset.Name)
		if err := download(ctx, asset.URL, bin); err != nil {
			return err
		}
		if err := os.Chmod(bin, 0o755); err != nil {
			return err
		}
		sum, err := fileSHA256(bin)
		if err != nil {
			return err
		}
		if sum != asset.SHA256 {
			return fmt.Errorf("%s: sha256 %s, want %s from the manifest", asset.Name, sum, asset.SHA256)
		}
		if cosign {
			if err := verify(ctx, m, asset.Bundle, bin, filepath.Join(out, asset.Name+".sigstore.json")); err != nil {
				return err
			}
			fmt.Printf("Verified %s: signed by %s\n", asset.Name, m.Signer.Identity)
		}
		name := scope + "/" + np.Package
		pkg := packageJSON{
			Name: name, Version: npmVersion, License: "SEE LICENSE IN LICENSE",
			Description: fmt.Sprintf("The %s binary of tetrate, the command for Tetrate products. Install %s/cli instead.", np.Platform, scope),
			Homepage:    repository, Repository: repo(),
			OS: []string{np.OS}, CPU: []string{np.CPU},
			Files:           []string{"bin/tetrate", "README.md", "LICENSE"},
			PreferUnplugged: true,
		}
		readme := fmt.Sprintf("# %s\n\nThe %s binary of `tetrate` %s. Install [%s/cli](https://www.npmjs.com/package/%s/cli), which picks the binary for your platform.\n", name, np.Platform, m.Version, scope, scope)
		if err := writePackage(pkgDir, pkg, readme, license); err != nil {
			return err
		}
		optional[name] = npmVersion
		order = append(order, np.Package)
	}

	// The main package and its launcher.
	main := packageJSON{
		Name: scope + "/cli", Version: npmVersion, License: "SEE LICENSE IN LICENSE",
		Description: "tetrate, the command for Tetrate products: install, verify and run them.",
		Homepage:    repository, Repository: repo(),
		Bin:                  map[string]string{"tetrate": "bin/tetrate.js"},
		OptionalDependencies: optional,
		Engines:              map[string]string{"node": ">=18"},
		Files:                []string{"bin/tetrate.js", "README.md", "LICENSE"},
	}
	if err := writePackage(filepath.Join(out, "cli"), main, mainReadme(m.Version), license); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "cli", "bin", "tetrate.js"), []byte(launcher), 0o755); err != nil {
		return err
	}
	order = append(order, "cli")

	// The unscoped wrapper.
	wrap := packageJSON{
		Name: wrapper, Version: npmVersion, License: "SEE LICENSE IN LICENSE",
		Description: fmt.Sprintf("tetrate, the command for Tetrate products. The same as %s/cli.", scope),
		Homepage:    repository, Repository: repo(),
		Bin:          map[string]string{"tetrate": "bin/tetrate.js"},
		Dependencies: map[string]string{scope + "/cli": npmVersion},
		Engines:      map[string]string{"node": ">=18"},
		Files:        []string{"bin/tetrate.js", "README.md", "LICENSE"},
	}
	if err := writePackage(filepath.Join(out, wrapper), wrap, mainReadme(m.Version), license); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, wrapper, "bin", "tetrate.js"), []byte(wrapperLauncher), 0o755); err != nil {
		return err
	}
	order = append(order, wrapper)

	var plan strings.Builder
	for _, d := range order {
		fmt.Fprintf(&plan, "%s %s\n", d, tag)
	}
	if err := os.WriteFile(filepath.Join(out, "publish.txt"), []byte(plan.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %d packages of %s %s in %s, dist-tag %s\n", len(order), product, npmVersion, out, tag)
	return nil
}

// distTag follows the installer: latest is the newest stable release, or the
// newest prerelease while no stable release exists. Any other prerelease is
// next.
func distTag(m manifest, idx index) string {
	if !m.Prerelease {
		return "latest"
	}
	if idx.Latest.Stable == nil {
		return "latest"
	}
	return "next"
}

type packageJSON struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Description          string            `json:"description"`
	License              string            `json:"license"`
	Homepage             string            `json:"homepage"`
	Repository           map[string]string `json:"repository"`
	OS                   []string          `json:"os,omitempty"`
	CPU                  []string          `json:"cpu,omitempty"`
	Bin                  map[string]string `json:"bin,omitempty"`
	Dependencies         map[string]string `json:"dependencies,omitempty"`
	OptionalDependencies map[string]string `json:"optionalDependencies,omitempty"`
	Engines              map[string]string `json:"engines,omitempty"`
	Files                []string          `json:"files"`
	PreferUnplugged      bool              `json:"preferUnplugged,omitempty"`
}

// repo names this repository, so npm provenance can match the publish run.
func repo() map[string]string {
	return map[string]string{"type": "git", "url": "git+" + repository + ".git"}
}

func writePackage(dir string, p packageJSON, readme string, license []byte) error {
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), b.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "LICENSE"), license, 0o644)
}

func mainReadme(version string) string {
	return fmt.Sprintf("# tetrate\n\n`tetrate` %s is the command for Tetrate products. It installs, verifies and runs them.\n\n```sh\nnpm install -g %s/cli\ntetrate install router\n```\n\nnpm installs only the binary for your platform: darwin-arm64, linux-x64 or linux-arm64. On another platform, or when optional dependencies are turned off, `tetrate` says so. Other ways to install: `curl -fsSL https://get.tetrate.io | sh` and `brew install tetrateio/tap/tetrate`.\n\nThe binaries are published in [tetrateio/releases](%s), where each one has a sigstore signature.\n", version, scope, repository)
}

// launcher runs the binary of the matching platform package.
const launcher = `#!/usr/bin/env node
"use strict";
// Runs the tetrate binary of the platform package that npm installed for this
// host. Generated by tools/npm in tetrateio/releases.
const { spawnSync } = require("node:child_process");
const fs = require("node:fs");

const packages = {
  "darwin arm64": "@tetrate/cli-darwin-arm64",
  "linux x64": "@tetrate/cli-linux-x64",
  "linux arm64": "@tetrate/cli-linux-arm64",
};

function fail(message) {
  process.stderr.write("tetrate: " + message + "\n");
  process.exit(1);
}

const pkg = packages[process.platform + " " + process.arch];
if (!pkg) {
  fail("there is no tetrate for " + process.platform + "-" + process.arch +
    ". Supported: darwin-arm64, linux-x64, linux-arm64.");
}
let bin;
try {
  bin = require.resolve(pkg + "/bin/tetrate");
} catch {
  fail(pkg + " is not installed. Install again without --omit=optional or --no-optional," +
    " or install tetrate with: curl -fsSL https://get.tetrate.io | sh");
}
try {
  fs.accessSync(bin, fs.constants.X_OK);
} catch {
  fs.chmodSync(bin, 0o755);
}
const result = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  fail("cannot run " + bin + ": " + result.error.message);
}
if (result.signal) {
  process.kill(process.pid, result.signal);
}
process.exit(result.status === null ? 1 : result.status);
`

// wrapperLauncher hands over to the launcher of the main package.
const wrapperLauncher = `#!/usr/bin/env node
"use strict";
// The unscoped tetrate package runs @tetrate/cli. Generated by tools/npm in
// tetrateio/releases.
require("@tetrate/cli/bin/tetrate.js");
`

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// download fetches a public URL with no credential.
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
// The bundle is kept outside the package directories.
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
