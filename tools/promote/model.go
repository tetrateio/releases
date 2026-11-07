package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Products is products.json: what this repository publishes.
type Products struct {
	SchemaVersion int                `json:"schemaVersion"`
	Products      map[string]Product `json:"products"`
}

// Product describes one product: where its releases are built and who signs
// them.
type Product struct {
	Summary   string    `json:"summary"`
	Source    string    `json:"source"`
	TagPrefix string    `json:"tagPrefix"`
	SourceTag string    `json:"sourceTag"`
	Platforms []string  `json:"platforms"`
	Asset     string    `json:"asset"`
	Checksums string    `json:"checksums"`
	Signature Signature `json:"signature"`
}

// Signature is how every file of a release is signed.
type Signature struct {
	Bundle   string `json:"bundle"`
	Issuer   string `json:"issuer"`
	Identity string `json:"identity"`
	Trigger  string `json:"trigger"`
}

func loadProducts(path string) (Products, error) {
	var p Products
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	if p.SchemaVersion != 1 {
		return p, fmt.Errorf("%s: schemaVersion %d is not 1", path, p.SchemaVersion)
	}
	return p, nil
}

// AssetName is the file name of the binary of a version for a platform.
func (p Product) AssetName(version, platform string) string {
	return strings.NewReplacer("{version}", version, "{platform}", platform).Replace(p.Asset)
}

// BundleName is the sigstore bundle of a file.
func (p Product) BundleName(asset string) string {
	return strings.ReplaceAll(p.Signature.Bundle, "{asset}", asset)
}

// Identity is the exact signer identity of a version.
func (p Product) Identity(version string) string {
	return strings.ReplaceAll(p.Signature.Identity, "{version}", version)
}

// CheckTag refuses a tag that the product does not release.
func (p Product) CheckTag(tag string) error {
	re, err := regexp.Compile(p.SourceTag)
	if err != nil {
		return fmt.Errorf("sourceTag: %w", err)
	}
	if !re.MatchString(tag) || !semver.IsValid(tag) {
		return fmt.Errorf("tag %q is not a release tag of %s", tag, p.Source)
	}
	return nil
}

// Manifest is <product>/<version>.json. It is written once.
type Manifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Product       string    `json:"product"`
	Version       string    `json:"version"`
	Prerelease    bool      `json:"prerelease"`
	PublishedAt   time.Time `json:"publishedAt"`
	Source        Source    `json:"source"`
	Signer        Signer    `json:"signer"`
	Release       string    `json:"release"`
	Assets        []Asset   `json:"assets"`
	Checksums     FileRef   `json:"checksums"`
	License       FileRef   `json:"license"`
}

// Source is where the release was built.
type Source struct {
	Repo        string    `json:"repo"`
	Tag         string    `json:"tag"`
	Commit      string    `json:"commit"`
	PublishedAt time.Time `json:"publishedAt"`
}

// Signer is the exact identity that signed every file.
type Signer struct {
	Issuer   string `json:"issuer"`
	Identity string `json:"identity"`
}

// Asset is one binary.
type Asset struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`
	Bundle   string `json:"bundle"`
}

// FileRef is a file of the release and its bundle, when it has one.
type FileRef struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Bundle string `json:"bundle,omitempty"`
}

// Index is <product>/index.json.
type Index struct {
	SchemaVersion int            `json:"schemaVersion"`
	Product       string         `json:"product"`
	Latest        Latest         `json:"latest"`
	Versions      []IndexVersion `json:"versions"`
}

// Latest names the newest stable release and the newest prerelease.
type Latest struct {
	Stable     *string `json:"stable"`
	Prerelease *string `json:"prerelease"`
}

// IndexVersion is one entry of the index.
type IndexVersion struct {
	Version     string    `json:"version"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"publishedAt"`
	Manifest    string    `json:"manifest"`
}

func manifestPath(dir, product, version string) string {
	return filepath.Join(dir, product, version+".json")
}

func indexPath(dir, product string) string {
	return filepath.Join(dir, product, "index.json")
}

func loadIndex(path, product string) (Index, error) {
	idx := Index{SchemaVersion: 1, Product: product}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return idx, nil
	}
	if err != nil {
		return idx, err
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return idx, fmt.Errorf("%s: %w", path, err)
	}
	if idx.Product != product {
		return idx, fmt.Errorf("%s names product %q, not %q", path, idx.Product, product)
	}
	return idx, nil
}

// Add puts a version into the index, keeps the versions newest first by
// semantic version and recomputes the latest stable and prerelease. Adding
// a version that is already there with the same manifest is a no-op.
func (idx *Index) Add(v IndexVersion) error {
	for _, e := range idx.Versions {
		if e.Version == v.Version {
			if e.Manifest != v.Manifest || e.Prerelease != v.Prerelease {
				return fmt.Errorf("index already has %s with other content", v.Version)
			}
			return nil
		}
	}
	idx.Versions = append(idx.Versions, v)
	sort.SliceStable(idx.Versions, func(i, j int) bool {
		return semver.Compare(idx.Versions[i].Version, idx.Versions[j].Version) > 0
	})
	idx.Latest = Latest{}
	for _, e := range idx.Versions {
		ver := e.Version
		if e.Prerelease {
			if idx.Latest.Prerelease == nil {
				idx.Latest.Prerelease = &ver
			}
		} else if idx.Latest.Stable == nil {
			idx.Latest.Stable = &ver
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
