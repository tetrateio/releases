package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// buildApt returns the files of the apt tree: the suite stable with the
// component main, one Packages index per architecture, and the Release
// file that lists their digests. Release is unsigned here.
func buildApt(debs []pkgFile, arches []string, date time.Time, downloads string) (map[string][]byte, error) {
	files := map[string][]byte{}
	byArch := map[string][]string{}
	for _, pk := range debs {
		control, err := debControl(pk.Path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pk.Name, err)
		}
		arch := controlField(control, "Architecture")
		if want := strings.TrimPrefix(pk.Platform, "linux-"); arch != want {
			return nil, fmt.Errorf("%s: Architecture %q, the manifest says %s", pk.Name, arch, pk.Platform)
		}
		for _, f := range []string{"Filename", "Size", "SHA256", "MD5sum"} {
			if controlField(control, f) != "" {
				return nil, fmt.Errorf("%s: the control file has the field %s", pk.Name, f)
			}
		}
		pool, ok := strings.CutPrefix(pk.URL, downloads)
		if !ok {
			return nil, fmt.Errorf("%s: the URL %s does not start with %s", pk.Name, pk.URL, downloads)
		}
		entry := strings.TrimRight(control, "\n") + "\n" +
			"Filename: pool/" + pool + "\n" +
			"Size: " + strconv.FormatInt(pk.Size, 10) + "\n" +
			"SHA256: " + pk.SHA256 + "\n"
		byArch[arch] = append(byArch[arch], entry)
	}

	for a := range byArch {
		if !slices.Contains(arches, a) {
			return nil, fmt.Errorf("a package has the architecture %s, which no product lists", a)
		}
	}
	var sums strings.Builder
	for _, arch := range arches {
		dir := "main/binary-" + arch + "/"
		packages := []byte(strings.Join(byArch[arch], "\n"))
		gz, err := gzipBytes(packages)
		if err != nil {
			return nil, err
		}
		for _, f := range []struct {
			name string
			data []byte
		}{{"Packages", packages}, {"Packages.gz", gz}} {
			sum := sha256Hex(f.data)
			files["dists/stable/"+dir+f.name] = f.data
			files["dists/stable/"+dir+"by-hash/SHA256/"+sum] = f.data
			fmt.Fprintf(&sums, " %s %d %s\n", sum, len(f.data), dir+f.name)
		}
	}

	release := "Origin: Tetrate\n" +
		"Label: Tetrate\n" +
		"Suite: stable\n" +
		"Codename: stable\n" +
		"Date: " + date.UTC().Format(time.RFC1123) + "\n" +
		"Architectures: " + strings.Join(arches, " ") + "\n" +
		"Components: main\n" +
		"Description: Tetrate packages\n" +
		"Acquire-By-Hash: yes\n" +
		"SHA256:\n" + sums.String()
	files["dists/stable/Release"] = []byte(release)
	return files, nil
}

// debControl returns the control file of a .deb: an ar archive whose
// member control.tar.gz holds ./control.
func debControl(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	const magic = "!<arch>\n"
	if !bytes.HasPrefix(b, []byte(magic)) {
		return "", errors.New("not an ar archive")
	}
	rest := b[len(magic):]
	for len(rest) >= 60 {
		name := strings.TrimSuffix(strings.TrimSpace(string(rest[:16])), "/")
		size, err := strconv.Atoi(strings.TrimSpace(string(rest[48:58])))
		if err != nil || size < 0 || 60+size > len(rest) {
			return "", errors.New("malformed ar member")
		}
		body := rest[60 : 60+size]
		rest = rest[min(len(rest), 60+size+size%2):]
		if name != "control.tar.gz" {
			if strings.HasPrefix(name, "control.tar") {
				return "", fmt.Errorf("%s is not supported, only control.tar.gz", name)
			}
			continue
		}
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return "", errors.New("control.tar.gz has no control file")
			}
			if err != nil {
				return "", err
			}
			if h.Name == "./control" || h.Name == "control" {
				c, err := io.ReadAll(io.LimitReader(tr, 1<<20))
				return string(c), err
			}
		}
	}
	return "", errors.New("no control.tar.gz")
}

// controlField returns the value of a single-line field of a control file.
func controlField(control, name string) string {
	for line := range strings.SplitSeq(control, "\n") {
		if v, ok := strings.CutPrefix(line, name+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// gzipBytes compresses with no name and no time in the header, so the same
// input gives the same bytes.
func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
