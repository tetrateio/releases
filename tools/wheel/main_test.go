package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPEP440(t *testing.T) {
	for tag, want := range map[string]string{
		"v0.1.0":         "0.1.0",
		"v0.1.0-alpha.3": "0.1.0a3",
		"v1.2.3-beta.10": "1.2.3b10",
		"v2.0.0-rc.1":    "2.0.0rc1",
	} {
		got, err := pep440(tag)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", tag, got, err, want)
		}
	}
	for _, bad := range []string{"0.1.0", "v0.1.0-dev.1", "v0.1.0-alpha", "v0.1.0-alpha.3.1", "v1.2"} {
		if _, err := pep440(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestWriteWheel(t *testing.T) {
	out := t.TempDir()
	tags := platformTags["linux-amd64"]
	name, err := writeWheel(out, "tetrate", "0.1.0a3", "v0.1.0-alpha.3", tags, []byte("#!/bin/sh\necho hi\n"), []byte("LICENSE TEXT\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "tetrate-0.1.0a3-py3-none-manylinux_2_17_x86_64.manylinux2014_x86_64.musllinux_1_2_x86_64.whl"; name != want {
		t.Errorf("name %s, want %s", name, want)
	}
	zr, err := zip.OpenReader(filepath.Join(out, name))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	read := func(path string) string {
		t.Helper()
		f, ok := files[path]
		if !ok {
			t.Fatalf("the wheel lacks %s", path)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		return string(b)
	}
	bin := files["tetrate-0.1.0a3.data/scripts/tetrate"]
	if bin == nil || bin.Mode().Perm() != 0o755 {
		t.Fatalf("the binary is missing or not executable: %v", bin)
	}
	wheel := read("tetrate-0.1.0a3.dist-info/WHEEL")
	for _, want := range []string{"Root-Is-Purelib: false", "Tag: py3-none-manylinux_2_17_x86_64", "Tag: py3-none-musllinux_1_2_x86_64"} {
		if !strings.Contains(wheel, want) {
			t.Errorf("WHEEL lacks %q:\n%s", want, wheel)
		}
	}
	meta := read("tetrate-0.1.0a3.dist-info/METADATA")
	for _, want := range []string{"Metadata-Version: 2.4\n", "Name: tetrate\n", "Version: 0.1.0a3\n", "License-File: LICENSE\n", "Classifier: License :: Other/Proprietary License\n", "uvx tetrate install router"} {
		if !strings.Contains(meta, want) {
			t.Errorf("METADATA lacks %q", want)
		}
	}
	if read("tetrate-0.1.0a3.dist-info/licenses/LICENSE") != "LICENSE TEXT\n" {
		t.Error("LICENSE differs")
	}
	// Every file but RECORD is in RECORD with its digest and size.
	record := read("tetrate-0.1.0a3.dist-info/RECORD")
	for path, f := range files {
		if path == "tetrate-0.1.0a3.dist-info/RECORD" {
			if !strings.Contains(record, path+",,\n") {
				t.Errorf("RECORD lacks its own line")
			}
			continue
		}
		body := read(path)
		h := sha256.Sum256([]byte(body))
		line := path + ",sha256=" + base64.RawURLEncoding.EncodeToString(h[:]) + ","
		if !strings.Contains(record, line) || f.UncompressedSize64 != uint64(len(body)) {
			t.Errorf("RECORD lacks %s", line)
		}
	}
	// The same inputs give the same bytes.
	first, _ := os.ReadFile(filepath.Join(out, name))
	if _, err := writeWheel(out, "tetrate", "0.1.0a3", "v0.1.0-alpha.3", tags, []byte("#!/bin/sh\necho hi\n"), []byte("LICENSE TEXT\n")); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(out, name))
	if string(first) != string(second) {
		t.Error("the wheel is not reproducible")
	}
}
