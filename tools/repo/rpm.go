package main

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// RPM header tags that the dnf metadata needs.
const (
	tagName           = 1000
	tagVersion        = 1001
	tagRelease        = 1002
	tagEpoch          = 1003
	tagSummary        = 1004
	tagDescription    = 1005
	tagBuildTime      = 1006
	tagBuildHost      = 1007
	tagSize           = 1009
	tagVendor         = 1011
	tagLicense        = 1014
	tagPackager       = 1015
	tagGroup          = 1016
	tagURL            = 1020
	tagArch           = 1022
	tagFileModes      = 1030
	tagSourceRPM      = 1044
	tagArchiveSize    = 1046
	tagProvideName    = 1047
	tagRequireFlags   = 1048
	tagRequireName    = 1049
	tagRequireVersion = 1050
	tagProvideFlags   = 1112
	tagProvideVersion = 1113
	tagDirIndexes     = 1116
	tagBaseNames      = 1117
	tagDirNames       = 1118
	tagLongSize       = 5009
	sigTagPayloadSize = 1007
)

// rpmHeader is a parsed header structure of an rpm file.
type rpmHeader struct {
	entries map[uint32]rpmEntry
	store   []byte
}

type rpmEntry struct {
	typ, offset, count uint32
}

// rpmInfo is what the dnf metadata says about one package.
type rpmInfo struct {
	Name, Arch, Epoch, Version, Release     string
	Summary, Description, Packager, URL     string
	License, Vendor, Group, BuildHost, Srpm string
	BuildTime                               int64
	InstalledSize, ArchiveSize              int64
	HeaderStart, HeaderEnd                  int64
	Provides, Requires                      []rpmDep
	Files                                   []rpmFile
}

type rpmDep struct {
	Name, Flags, Epoch, Version, Release string
}

type rpmFile struct {
	Path string
	Dir  bool
}

// readRPM reads the signature header and the header of an rpm file.
func readRPM(file string) (rpmInfo, error) {
	var info rpmInfo
	b, err := os.ReadFile(file)
	if err != nil {
		return info, err
	}
	if len(b) < 96 || !bytes.Equal(b[:4], []byte{0xed, 0xab, 0xee, 0xdb}) {
		return info, errors.New("not an rpm file")
	}
	sig, end, err := readHeader(b, 96)
	if err != nil {
		return info, fmt.Errorf("signature header: %w", err)
	}
	start := (end + 7) &^ 7
	h, end, err := readHeader(b, start)
	if err != nil {
		return info, fmt.Errorf("header: %w", err)
	}
	info.HeaderStart, info.HeaderEnd = int64(start), int64(end)
	info.Name = h.str(tagName)
	info.Arch = h.str(tagArch)
	info.Version = h.str(tagVersion)
	info.Release = h.str(tagRelease)
	info.Epoch = "0"
	if e := h.ints(tagEpoch); len(e) > 0 {
		info.Epoch = strconv.FormatInt(e[0], 10)
	}
	info.Summary = h.str(tagSummary)
	info.Description = h.str(tagDescription)
	info.Packager = h.str(tagPackager)
	info.URL = h.str(tagURL)
	info.License = h.str(tagLicense)
	info.Vendor = h.str(tagVendor)
	info.Group = h.str(tagGroup)
	info.BuildHost = h.str(tagBuildHost)
	info.Srpm = h.str(tagSourceRPM)
	if t := h.ints(tagBuildTime); len(t) > 0 {
		info.BuildTime = t[0]
	}
	if s := h.ints(tagLongSize); len(s) > 0 {
		info.InstalledSize = s[0]
	} else if s := h.ints(tagSize); len(s) > 0 {
		info.InstalledSize = s[0]
	}
	if s := h.ints(tagArchiveSize); len(s) > 0 {
		info.ArchiveSize = s[0]
	} else if s := sig.ints(sigTagPayloadSize); len(s) > 0 {
		info.ArchiveSize = s[0]
	}
	if info.Name == "" || info.Version == "" || info.Arch == "" {
		return info, errors.New("the header has no name, version or arch")
	}
	info.Provides = deps(h.strs(tagProvideName), h.ints(tagProvideFlags), h.strs(tagProvideVersion))
	for _, d := range deps(h.strs(tagRequireName), h.ints(tagRequireFlags), h.strs(tagRequireVersion)) {
		// createrepo leaves out the capabilities of rpm itself.
		if !strings.HasPrefix(d.Name, "rpmlib(") {
			info.Requires = append(info.Requires, d)
		}
	}
	dirs, bases, idx, modes := h.strs(tagDirNames), h.strs(tagBaseNames), h.ints(tagDirIndexes), h.ints(tagFileModes)
	if len(idx) != len(bases) {
		return info, errors.New("the file list is malformed")
	}
	for i, base := range bases {
		if idx[i] < 0 || int(idx[i]) >= len(dirs) {
			return info, errors.New("the file list is malformed")
		}
		f := rpmFile{Path: dirs[idx[i]] + base}
		if i < len(modes) && modes[i]&0o170000 == 0o040000 {
			f.Dir = true
		}
		info.Files = append(info.Files, f)
	}
	return info, nil
}

func readHeader(b []byte, off int) (rpmHeader, int, error) {
	var h rpmHeader
	if off+16 > len(b) || !bytes.Equal(b[off:off+4], []byte{0x8e, 0xad, 0xe8, 0x01}) {
		return h, 0, errors.New("no header magic")
	}
	n := int(binary.BigEndian.Uint32(b[off+8:]))
	size := int(binary.BigEndian.Uint32(b[off+12:]))
	index := off + 16
	store := index + 16*n
	end := store + size
	if n > 1<<16 || size > 1<<28 || end > len(b) {
		return h, 0, errors.New("the header is larger than the file")
	}
	h.store = b[store:end]
	h.entries = map[uint32]rpmEntry{}
	for i := range n {
		e := b[index+16*i:]
		h.entries[binary.BigEndian.Uint32(e)] = rpmEntry{
			typ:    binary.BigEndian.Uint32(e[4:]),
			offset: binary.BigEndian.Uint32(e[8:]),
			count:  binary.BigEndian.Uint32(e[12:]),
		}
	}
	return h, end, nil
}

// strs returns the strings of a STRING, STRING_ARRAY or I18NSTRING entry.
func (h rpmHeader) strs(tag uint32) []string {
	e, ok := h.entries[tag]
	if !ok || (e.typ != 6 && e.typ != 8 && e.typ != 9) || int(e.offset) > len(h.store) {
		return nil
	}
	count := int(e.count)
	if e.typ == 6 {
		count = 1
	}
	var out []string
	rest := h.store[e.offset:]
	for range count {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return out
		}
		out = append(out, string(rest[:i]))
		rest = rest[i+1:]
	}
	return out
}

func (h rpmHeader) str(tag uint32) string {
	if s := h.strs(tag); len(s) > 0 {
		return s[0]
	}
	return ""
}

// ints returns the values of an INT16, INT32 or INT64 entry.
func (h rpmHeader) ints(tag uint32) []int64 {
	e, ok := h.entries[tag]
	if !ok {
		return nil
	}
	width := map[uint32]int{3: 2, 4: 4, 5: 8}[e.typ]
	if width == 0 || int(e.offset)+width*int(e.count) > len(h.store) {
		return nil
	}
	out := make([]int64, e.count)
	for i := range out {
		v := h.store[int(e.offset)+width*i:]
		switch width {
		case 2:
			out[i] = int64(binary.BigEndian.Uint16(v))
		case 4:
			out[i] = int64(binary.BigEndian.Uint32(v))
		case 8:
			out[i] = int64(binary.BigEndian.Uint64(v))
		}
	}
	return out
}

func deps(names []string, flags []int64, versions []string) []rpmDep {
	var out []rpmDep
	for i, n := range names {
		d := rpmDep{Name: n}
		if i < len(flags) && i < len(versions) && versions[i] != "" {
			d.Flags = map[int64]string{2: "LT", 4: "GT", 8: "EQ", 10: "LE", 12: "GE"}[flags[i]&0xf]
			evr := versions[i]
			d.Epoch = "0"
			if e, rest, ok := strings.Cut(evr, ":"); ok {
				d.Epoch, evr = e, rest
			}
			d.Version, d.Release = evr, ""
			if j := strings.LastIndex(evr, "-"); j >= 0 {
				d.Version, d.Release = evr[:j], evr[j+1:]
			}
		}
		out = append(out, d)
	}
	return out
}

// dnfArch is the rpm name of a deb architecture.
var dnfArch = map[string]string{"amd64": "x86_64", "arm64": "aarch64"}

// buildDnf returns the files of one dnf repository per architecture:
// <arch>/repodata/repomd.xml and the primary, filelists and other files
// it names by digest. repomd.xml is unsigned here.
func buildDnf(rpms []pkgFile, arches []string, date time.Time) (map[string][]byte, error) {
	files := map[string][]byte{}
	type entry struct {
		pk   pkgFile
		info rpmInfo
	}
	byArch := map[string][]entry{}
	for _, pk := range rpms {
		info, err := readRPM(pk.Path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pk.Name, err)
		}
		if want := dnfArch[strings.TrimPrefix(pk.Platform, "linux-")]; info.Arch != want {
			return nil, fmt.Errorf("%s: arch %q, the manifest says %s", pk.Name, info.Arch, pk.Platform)
		}
		byArch[info.Arch] = append(byArch[info.Arch], entry{pk, info})
	}
	for _, a := range arches {
		arch, ok := dnfArch[a]
		if !ok {
			return nil, fmt.Errorf("no rpm name for the architecture %s", a)
		}
		var primary, filelists, other strings.Builder
		n := len(byArch[arch])
		fmt.Fprintf(&primary, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata xmlns=\"http://linux.duke.edu/metadata/common\" xmlns:rpm=\"http://linux.duke.edu/metadata/rpm\" packages=\"%d\">\n", n)
		fmt.Fprintf(&filelists, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<filelists xmlns=\"http://linux.duke.edu/metadata/filelists\" packages=\"%d\">\n", n)
		fmt.Fprintf(&other, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<otherdata xmlns=\"http://linux.duke.edu/metadata/other\" packages=\"%d\">\n", n)
		for _, e := range byArch[arch] {
			writePrimary(&primary, e.pk, e.info, date)
			ver := fmt.Sprintf("<version epoch=\"%s\" ver=\"%s\" rel=\"%s\"/>", e.info.Epoch, esc(e.info.Version), esc(e.info.Release))
			fmt.Fprintf(&filelists, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n  %s\n", e.pk.SHA256, esc(e.info.Name), arch, ver)
			for _, f := range e.info.Files {
				if f.Dir {
					fmt.Fprintf(&filelists, "  <file type=\"dir\">%s</file>\n", esc(f.Path))
				} else {
					fmt.Fprintf(&filelists, "  <file>%s</file>\n", esc(f.Path))
				}
			}
			filelists.WriteString("</package>\n")
			fmt.Fprintf(&other, "<package pkgid=\"%s\" name=\"%s\" arch=\"%s\">\n  %s\n</package>\n", e.pk.SHA256, esc(e.info.Name), arch, ver)
		}
		primary.WriteString("</metadata>\n")
		filelists.WriteString("</filelists>\n")
		other.WriteString("</otherdata>\n")

		var repomd strings.Builder
		fmt.Fprintf(&repomd, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<repomd xmlns=\"http://linux.duke.edu/metadata/repo\" xmlns:rpm=\"http://linux.duke.edu/metadata/rpm\">\n  <revision>%d</revision>\n", date.Unix())
		for _, d := range []struct {
			kind string
			xml  string
		}{{"primary", primary.String()}, {"filelists", filelists.String()}, {"other", other.String()}} {
			gz, err := gzipBytes([]byte(d.xml))
			if err != nil {
				return nil, err
			}
			sum := sha256Hex(gz)
			name := "repodata/" + sum + "-" + d.kind + ".xml.gz"
			files[arch+"/"+name] = gz
			fmt.Fprintf(&repomd, "  <data type=\"%s\">\n    <checksum type=\"sha256\">%s</checksum>\n    <open-checksum type=\"sha256\">%s</open-checksum>\n    <location href=\"%s\"/>\n    <timestamp>%d</timestamp>\n    <size>%d</size>\n    <open-size>%d</open-size>\n  </data>\n",
				d.kind, sum, sha256Hex([]byte(d.xml)), name, date.Unix(), len(gz), len(d.xml))
		}
		repomd.WriteString("</repomd>\n")
		files[arch+"/repodata/repomd.xml"] = []byte(repomd.String())
	}
	return files, nil
}

func writePrimary(b *strings.Builder, pk pkgFile, info rpmInfo, date time.Time) {
	base, href := path.Split(pk.URL)
	fmt.Fprintf(b, "<package type=\"rpm\">\n  <name>%s</name>\n  <arch>%s</arch>\n", esc(info.Name), esc(info.Arch))
	fmt.Fprintf(b, "  <version epoch=\"%s\" ver=\"%s\" rel=\"%s\"/>\n", info.Epoch, esc(info.Version), esc(info.Release))
	fmt.Fprintf(b, "  <checksum type=\"sha256\" pkgid=\"YES\">%s</checksum>\n", pk.SHA256)
	fmt.Fprintf(b, "  <summary>%s</summary>\n  <description>%s</description>\n", esc(info.Summary), esc(info.Description))
	fmt.Fprintf(b, "  <packager>%s</packager>\n  <url>%s</url>\n", esc(info.Packager), esc(info.URL))
	fmt.Fprintf(b, "  <time file=\"%d\" build=\"%d\"/>\n", date.Unix(), info.BuildTime)
	fmt.Fprintf(b, "  <size package=\"%d\" installed=\"%d\" archive=\"%d\"/>\n", pk.Size, info.InstalledSize, info.ArchiveSize)
	fmt.Fprintf(b, "  <location xml:base=\"%s\" href=\"%s\"/>\n", esc(base), esc(href))
	b.WriteString("  <format>\n")
	fmt.Fprintf(b, "    <rpm:license>%s</rpm:license>\n    <rpm:vendor>%s</rpm:vendor>\n    <rpm:group>%s</rpm:group>\n", esc(info.License), esc(info.Vendor), esc(info.Group))
	fmt.Fprintf(b, "    <rpm:buildhost>%s</rpm:buildhost>\n    <rpm:sourcerpm>%s</rpm:sourcerpm>\n", esc(info.BuildHost), esc(info.Srpm))
	fmt.Fprintf(b, "    <rpm:header-range start=\"%d\" end=\"%d\"/>\n", info.HeaderStart, info.HeaderEnd)
	for _, list := range []struct {
		tag  string
		deps []rpmDep
	}{{"provides", info.Provides}, {"requires", info.Requires}} {
		if len(list.deps) == 0 {
			continue
		}
		fmt.Fprintf(b, "    <rpm:%s>\n", list.tag)
		for _, d := range list.deps {
			if d.Flags == "" {
				fmt.Fprintf(b, "      <rpm:entry name=\"%s\"/>\n", esc(d.Name))
				continue
			}
			fmt.Fprintf(b, "      <rpm:entry name=\"%s\" flags=\"%s\" epoch=\"%s\" ver=\"%s\"", esc(d.Name), d.Flags, esc(d.Epoch), esc(d.Version))
			if d.Release != "" {
				fmt.Fprintf(b, " rel=\"%s\"", esc(d.Release))
			}
			b.WriteString("/>\n")
		}
		fmt.Fprintf(b, "    </rpm:%s>\n", list.tag)
	}
	// primary lists only the files that dependencies usually name.
	for _, f := range info.Files {
		if strings.HasPrefix(f.Path, "/etc/") || strings.Contains(f.Path, "bin/") || f.Path == "/usr/lib/sendmail" {
			fmt.Fprintf(b, "    <file>%s</file>\n", esc(f.Path))
		}
	}
	b.WriteString("  </format>\n</package>\n")
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
