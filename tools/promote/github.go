package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// GitHub is a small client for the release API calls the tool needs.
type GitHub struct {
	API    string
	Upload string
	Token  string
	HTTP   *http.Client
}

// Release is the part of a GitHub release the tool reads.
type Release struct {
	ID          int64          `json:"id"`
	Tag         string         `json:"tag_name"`
	Target      string         `json:"target_commitish"`
	Name        string         `json:"name"`
	Body        string         `json:"body"`
	Draft       bool           `json:"draft"`
	Prerelease  bool           `json:"prerelease"`
	PublishedAt time.Time      `json:"published_at"`
	HTMLURL     string         `json:"html_url"`
	Assets      []ReleaseAsset `json:"assets"`
}

// ReleaseAsset is one file of a release.
type ReleaseAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

func newGitHub() (*GitHub, error) {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		out, err := exec.Command("gh", "auth", "token").Output()
		if err != nil {
			return nil, errors.New("set GITHUB_TOKEN or log in with gh auth login")
		}
		tok = strings.TrimSpace(string(out))
	}
	return &GitHub{API: "https://api.github.com", Upload: "https://uploads.github.com", Token: tok, HTTP: &http.Client{Timeout: 10 * time.Minute}}, nil
}

func (g *GitHub) do(ctx context.Context, method, u, accept string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &StatusError{Code: resp.StatusCode, Method: method, URL: u, Body: strings.TrimSpace(string(msg))}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// StatusError is an API answer that is not a success.
type StatusError struct {
	Code        int
	Method, URL string
	Body        string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Code, e.Body)
}

// ReleaseByTag reads a published release.
func (g *GitHub) ReleaseByTag(ctx context.Context, repo, tag string) (Release, error) {
	var r Release
	err := g.do(ctx, http.MethodGet, g.API+"/repos/"+repo+"/releases/tags/"+url.PathEscape(tag), "application/vnd.github+json", nil, "", &r)
	return r, err
}

// ReleaseByTagAny finds a release by tag, drafts included, which the tags
// endpoint does not return.
func (g *GitHub) ReleaseByTagAny(ctx context.Context, repo, tag string) (*Release, error) {
	for page := 1; page <= 20; page++ {
		var rs []Release
		if err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", g.API, repo, page), "application/vnd.github+json", nil, "", &rs); err != nil {
			return nil, err
		}
		for i := range rs {
			if rs[i].Tag == tag {
				return &rs[i], nil
			}
		}
		if len(rs) < 100 {
			return nil, nil
		}
	}
	return nil, nil
}

// CommitOfTag resolves a tag to its commit, through an annotated tag object.
func (g *GitHub) CommitOfTag(ctx context.Context, repo, tag string) (string, error) {
	var ref struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := g.do(ctx, http.MethodGet, g.API+"/repos/"+repo+"/git/ref/tags/"+url.PathEscape(tag), "application/vnd.github+json", nil, "", &ref); err != nil {
		return "", err
	}
	sha, typ := ref.Object.SHA, ref.Object.Type
	for range 4 {
		if typ == "commit" {
			return sha, nil
		}
		var obj struct {
			Object struct {
				SHA  string `json:"sha"`
				Type string `json:"type"`
			} `json:"object"`
		}
		if err := g.do(ctx, http.MethodGet, g.API+"/repos/"+repo+"/git/tags/"+sha, "application/vnd.github+json", nil, "", &obj); err != nil {
			return "", err
		}
		sha, typ = obj.Object.SHA, obj.Object.Type
	}
	return "", fmt.Errorf("tag %s does not resolve to a commit", tag)
}

// Download writes an asset to path.
func (g *GitHub) Download(ctx context.Context, a ReleaseAsset, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := g.do(ctx, http.MethodGet, a.URL, "application/octet-stream", nil, "", f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// CreateDraft creates a draft release. GitHub creates the tag only when the
// release is published.
func (g *GitHub) CreateDraft(ctx context.Context, repo, tag, name, body string, prerelease bool) (Release, error) {
	in, _ := json.Marshal(map[string]any{"tag_name": tag, "name": name, "body": body, "draft": true, "prerelease": prerelease})
	var r Release
	err := g.do(ctx, http.MethodPost, g.API+"/repos/"+repo+"/releases", "application/vnd.github+json", bytes.NewReader(in), "application/json", &r)
	return r, err
}

// UploadAsset adds a file to a release.
func (g *GitHub) UploadAsset(ctx context.Context, repo string, releaseID int64, name, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/repos/%s/releases/%d/assets?name=%s", g.Upload, repo, releaseID, url.QueryEscape(name))
	return g.do(ctx, http.MethodPost, u, "application/vnd.github+json", bytes.NewReader(b), "application/octet-stream", nil)
}

// Publish points a draft at a commit and publishes it, which creates the tag.
func (g *GitHub) Publish(ctx context.Context, repo string, releaseID int64, commit string, latest bool) (Release, error) {
	makeLatest := "false"
	if latest {
		makeLatest = "true"
	}
	in, _ := json.Marshal(map[string]any{"draft": false, "target_commitish": commit, "make_latest": makeLatest})
	var r Release
	err := g.do(ctx, http.MethodPatch, fmt.Sprintf("%s/repos/%s/releases/%d", g.API, repo, releaseID), "application/vnd.github+json", bytes.NewReader(in), "application/json", &r)
	return r, err
}

// DeleteRelease removes a draft after a failure.
func (g *GitHub) DeleteRelease(ctx context.Context, repo string, releaseID int64) error {
	return g.do(ctx, http.MethodDelete, fmt.Sprintf("%s/repos/%s/releases/%d", g.API, repo, releaseID), "application/vnd.github+json", nil, "", nil)
}
