// Command promote publishes a release of a product in this repository.
//
// It downloads the release from the product's private source repository,
// verifies every file against the checksums and the exact sigstore signer
// in products.json, creates the release here as a draft with the same
// files, commits the manifest and the index, and then publishes the
// release on that commit. Run it in a clean checkout of main:
//
//	go run ./tools/promote -product tetrate -tag v0.1.0-alpha.3
//
// The token comes from GITHUB_TOKEN, or from "gh auth token". The commit
// is made with git, so it carries the committer and the signature of
// whoever runs the tool. -dry-run downloads and verifies, then prints the
// manifest and the index without changing anything.
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

func main() {
	product := flag.String("product", "", "the product to promote, a key of products.json")
	tag := flag.String("tag", "", "the release tag in the source repository, such as v0.1.0-alpha.3")
	dir := flag.String("dir", ".", "the checkout of this repository")
	repo := flag.String("repo", "tetrateio/releases", "this repository")
	dryRun := flag.Bool("dry-run", false, "download and verify, print the manifest and the index, change nothing")
	flag.Parse()
	if *product == "" || *tag == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: promote -product <name> -tag <vX.Y.Z[-pre]> [-dry-run]")
		os.Exit(2)
	}
	ctx := context.Background()
	if err := run(ctx, *dir, *repo, *product, *tag, *dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "promote:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dir, repo, name, tag string, dryRun bool) error {
	products, err := loadProducts(filepath.Join(dir, "products.json"))
	if err != nil {
		return err
	}
	p, ok := products.Products[name]
	if !ok {
		return fmt.Errorf("products.json has no product %q", name)
	}
	if err := p.CheckTag(tag); err != nil {
		return err
	}
	if _, err := exec.LookPath("cosign"); err != nil {
		return errors.New("cosign is required on PATH to verify the signatures")
	}
	if !dryRun {
		if err := checkCheckout(dir); err != nil {
			return err
		}
	}
	mPath := manifestPath(dir, name, tag)
	if _, err := os.Stat(mPath); err == nil {
		return fmt.Errorf("%s exists: %s %s is already promoted. A manifest is never rewritten", mPath, name, tag)
	}

	gh, err := newGitHub()
	if err != nil {
		return err
	}
	src, err := gh.ReleaseByTag(ctx, p.Source, tag)
	if err != nil {
		return fmt.Errorf("read release %s of %s: %w", tag, p.Source, err)
	}
	if src.Draft {
		return fmt.Errorf("release %s of %s is a draft", tag, p.Source)
	}
	commit, err := gh.CommitOfTag(ctx, p.Source, tag)
	if err != nil {
		return fmt.Errorf("resolve tag %s: %w", tag, err)
	}

	work, err := os.MkdirTemp("", "promote-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	// The files of the release, each with its bundle.
	var files []string
	for _, pl := range p.Platforms {
		files = append(files, p.AssetName(tag, pl))
	}
	files = append(files, p.Checksums)
	var want []string
	for _, f := range files {
		want = append(want, f, p.BundleName(f))
	}
	byName := map[string]ReleaseAsset{}
	for _, a := range src.Assets {
		byName[a.Name] = a
	}
	for _, f := range want {
		a, ok := byName[f]
		if !ok {
			return fmt.Errorf("release %s of %s has no %s", tag, p.Source, f)
		}
		fmt.Printf("Downloading %s\n", f)
		if err := gh.Download(ctx, a, filepath.Join(work, f)); err != nil {
			return fmt.Errorf("download %s: %w", f, err)
		}
	}

	// Checksums, then the signer of every file.
	sums, err := readChecksums(filepath.Join(work, p.Checksums))
	if err != nil {
		return err
	}
	identity := p.Identity(tag)
	for _, f := range files {
		if f != p.Checksums {
			got, err := fileSHA256(filepath.Join(work, f))
			if err != nil {
				return err
			}
			if sums[f] == "" {
				return fmt.Errorf("%s has no line for %s", p.Checksums, f)
			}
			if got != sums[f] {
				return fmt.Errorf("%s does not match %s: got %s, want %s", f, p.Checksums, got, sums[f])
			}
		}
		if err := cosignVerify(ctx, p, work, f, identity, tag); err != nil {
			return err
		}
		fmt.Printf("Verified %s: signed by %s\n", f, identity)
	}

	m, err := buildManifest(p, name, repo, tag, src, commit, work, time.Now().UTC())
	if err != nil {
		return err
	}
	idx, err := loadIndex(indexPath(dir, name), name)
	if err != nil {
		return err
	}
	if err := idx.Add(IndexVersion{Version: tag, Prerelease: m.Prerelease, PublishedAt: m.PublishedAt, Manifest: tag + ".json"}); err != nil {
		return err
	}
	if dryRun {
		out, _ := json.MarshalIndent(map[string]any{"manifest": m, "index": idx}, "", "  ")
		fmt.Println(string(out))
		fmt.Println("Dry run: nothing changed.")
		return nil
	}

	// Draft first, so the files exist before the index points at them.
	release := p.TagPrefix + tag
	if old, err := gh.ReleaseByTagAny(ctx, repo, release); err != nil {
		return err
	} else if old != nil {
		if !old.Draft {
			return fmt.Errorf("%s already has a published release %s but no manifest. Check it by hand", repo, release)
		}
		fmt.Printf("Removing the draft %s left by an earlier run\n", release)
		if err := gh.DeleteRelease(ctx, repo, old.ID); err != nil {
			return err
		}
	}
	body := releaseBody(name, tag, p.Source, commit, src.Body)
	draft, err := gh.CreateDraft(ctx, repo, release, name+" "+tag, body, src.Prerelease)
	if err != nil {
		return fmt.Errorf("create the draft release: %w", err)
	}
	cleanup := func(cause error) error {
		if derr := gh.DeleteRelease(ctx, repo, draft.ID); derr != nil {
			return fmt.Errorf("%w (and the draft %s could not be removed: %v)", cause, release, derr)
		}
		return cause
	}
	uploads := append(append([]string{}, want...), "LICENSE")
	for _, f := range uploads {
		path := filepath.Join(work, f)
		if f == "LICENSE" {
			path = filepath.Join(dir, "LICENSE")
		}
		fmt.Printf("Uploading %s\n", f)
		if err := gh.UploadAsset(ctx, repo, draft.ID, f, path); err != nil {
			return cleanup(fmt.Errorf("upload %s: %w", f, err))
		}
	}

	if err := writeJSON(mPath, m); err != nil {
		return cleanup(err)
	}
	if err := writeJSON(indexPath(dir, name), idx); err != nil {
		return cleanup(err)
	}
	sha, err := commitAndPush(dir, name, fmt.Sprintf("Add %s %s", name, tag))
	if err != nil {
		return cleanup(err)
	}
	pub, err := gh.Publish(ctx, repo, draft.ID, sha, false)
	if err != nil {
		return fmt.Errorf("publish %s on %s: %w. The manifest is pushed; publish the draft by hand", release, sha, err)
	}
	fmt.Printf("Published %s\n", pub.HTMLURL)

	// The files as the public sees them.
	for _, a := range m.Assets {
		if err := checkPublic(ctx, a.URL, a.SHA256); err != nil {
			return err
		}
	}
	fmt.Printf("Checked the public downloads of %d files\n", len(m.Assets))
	return nil
}

func releaseBody(name, tag, source, commit, notes string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s, promoted from release %s of %s (commit %s).\n\n", name, tag, tag, source, commit)
	fmt.Fprintf(&b, "Every file is signed by the release workflow of %s. The manifest is [%s/%s.json](https://github.com/tetrateio/releases/blob/main/%s/%s.json).\n\n", source, name, tag, name, tag)
	if notes = strings.TrimSpace(notes); notes != "" {
		b.WriteString("---\n\n")
		b.WriteString(notes)
		b.WriteString("\n")
	}
	return b.String()
}

func buildManifest(p Product, name, repo, tag string, src Release, commit, work string, now time.Time) (Manifest, error) {
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s%s/", repo, p.TagPrefix, tag)
	m := Manifest{
		SchemaVersion: 1,
		Product:       name,
		Version:       tag,
		Prerelease:    src.Prerelease,
		PublishedAt:   now,
		Source:        Source{Repo: p.Source, Tag: tag, Commit: commit, PublishedAt: src.PublishedAt.UTC()},
		Signer:        Signer{Issuer: p.Signature.Issuer, Identity: p.Identity(tag)},
		Release:       fmt.Sprintf("https://github.com/%s/releases/tag/%s%s", repo, p.TagPrefix, tag),
		Checksums:     FileRef{Name: p.Checksums, URL: base + p.Checksums, Bundle: base + p.BundleName(p.Checksums)},
		License:       FileRef{Name: "LICENSE", URL: base + "LICENSE"},
	}
	for _, pl := range p.Platforms {
		f := p.AssetName(tag, pl)
		sum, err := fileSHA256(filepath.Join(work, f))
		if err != nil {
			return m, err
		}
		fi, err := os.Stat(filepath.Join(work, f))
		if err != nil {
			return m, err
		}
		m.Assets = append(m.Assets, Asset{Name: f, Platform: pl, SHA256: sum, Size: fi.Size(), URL: base + f, Bundle: base + p.BundleName(f)})
	}
	return m, nil
}

func readChecksums(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("%s: malformed line %q", filepath.Base(path), line)
		}
		sums[strings.TrimPrefix(f[1], "*")] = f[0]
	}
	return sums, nil
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

func cosignVerify(ctx context.Context, p Product, work, file, identity, tag string) error {
	cmd := exec.CommandContext(ctx, "cosign", "verify-blob",
		"--bundle", filepath.Join(work, p.BundleName(file)),
		"--certificate-identity", identity,
		"--certificate-oidc-issuer", p.Signature.Issuer,
		"--certificate-github-workflow-repository", p.Source,
		"--certificate-github-workflow-ref", "refs/tags/"+tag,
		"--certificate-github-workflow-trigger", p.Signature.Trigger,
		filepath.Join(work, file))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the signature of %s does not verify: %s", file, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// checkCheckout requires a clean main that equals origin/main, so the
// commit lands on top of the published history.
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

func commitAndPush(dir, product, msg string) (string, error) {
	if _, err := git(dir, "add", "--", product); err != nil {
		return "", err
	}
	if _, err := git(dir, "commit", "-S", "-m", msg); err != nil {
		return "", err
	}
	if _, err := git(dir, "push", "origin", "HEAD:main"); err != nil {
		if _, rerr := git(dir, "reset", "--hard", "HEAD~1"); rerr != nil {
			return "", fmt.Errorf("%w (and the local commit could not be undone: %v)", err, rerr)
		}
		return "", err
	}
	return git(dir, "rev-parse", "HEAD")
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

// checkPublic downloads a file anonymously and compares its digest.
func checkPublic(ctx context.Context, u, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("public download %s: HTTP %d", u, resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("public download %s: sha256 %s, want %s", u, got, want)
	}
	return nil
}
