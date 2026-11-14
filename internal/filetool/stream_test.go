package filetool

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runInput(input io.Reader, args ...string) ([]byte, string, int) {
	var stdout, stderr bytes.Buffer
	code := Run(args, input, &stdout, &stderr)
	return stdout.Bytes(), stderr.String(), code
}

func TestLargeBinaryWriteReadAndStat(t *testing.T) {
	name := filepath.Join(t.TempDir(), "binary 雪.dat")
	data := bytes.Repeat([]byte{0, 1, 2, 10, 13, 255, 42, 0}, 1300000)
	_, stderr, code := runInput(bytes.NewReader(data), "write", name, strconv.Itoa(len(data)), "0")
	if code != 0 {
		t.Fatalf("write exit %d: %s", code, stderr)
	}
	output, stderr, code := runInput(strings.NewReader(""), "read", name)
	if code != 0 || !bytes.Equal(output, data) {
		t.Fatalf("read exit %d, bytes %d: %s", code, len(output), stderr)
	}
	output, stderr, code = runInput(strings.NewReader(""), "stat", name)
	var stat struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
	}
	if code != 0 || json.Unmarshal(output, &stat) != nil || stat.Type != "file" || stat.Size != int64(len(data)) {
		t.Fatalf("stat exit %d, output %q: %s", code, output, stderr)
	}
}

func TestWriteShortInputKeepsExistingFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "existing")
	if err := os.WriteFile(name, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runInput(strings.NewReader("short"), "write", name, "10", "0")
	if code == 0 || !strings.Contains(stderr, "input bytes") {
		t.Fatalf("short write exit %d: %s", code, stderr)
	}
	content, err := os.ReadFile(name)
	if err != nil || string(content) != "original" {
		t.Fatalf("short write changed existing file: %q, %v", content, err)
	}
	for _, args := range [][]string{
		{"write", name, "-1", "0"},
		{"write", name, "0", "512"},
		{"read", filepath.Dir(name)},
	} {
		if _, stderr, code := runInput(strings.NewReader(""), args...); code == 0 || stderr == "" {
			t.Errorf("args %q exit %d: %s", args, code, stderr)
		}
	}
}

func TestWriteDoesNotWaitForEndOfInput(t *testing.T) {
	name := filepath.Join(t.TempDir(), "file")
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan int, 1)
	go func() {
		_, _, code := runInput(reader, "write", name, "3", "0")
		done <- code
	}()
	if _, err := writer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("write exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write waited for EOF after receiving its declared size")
	}
}

func TestExtractDoesNotWaitForEndOfInput(t *testing.T) {
	data := makeArchive(t, archiveItem{name: "file", kind: tar.TypeReg, data: []byte("content")})
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan int, 1)
	dest := t.TempDir()
	go func() {
		_, _, code := runInput(reader, "extract", dest, strconv.Itoa(len(data)))
		done <- code
	}()
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("extract exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("extract waited for EOF after receiving its declared size")
	}
	if content, err := os.ReadFile(filepath.Join(dest, "file")); err != nil || string(content) != "content" {
		t.Fatalf("extracted file = %q, %v", content, err)
	}
}

type archiveItem struct {
	name, link string
	kind       byte
	data       []byte
}

func makeArchive(t *testing.T, items ...archiveItem) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, item := range items {
		h := &tar.Header{Name: item.name, Typeflag: item.kind, Linkname: item.link, Mode: 0644, Size: int64(len(item.data)), ModTime: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)}
		if item.kind == tar.TypeDir {
			h.Mode = 0755
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestArchiveAndExtractPreserveContentsAndHardlinks(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source 雪")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0, 255, 3, 42}, 2500000)
	first := filepath.Join(source, "a.dat")
	if err := os.WriteFile(first, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(source, "b.dat")); err != nil {
		t.Fatalf("create hardlink: %v", err)
	}
	archiveBytes, stderr, code := runInput(strings.NewReader(""), "archive", source)
	if code != 0 {
		t.Fatalf("archive exit %d: %s", code, stderr)
	}
	if err := ValidateArchive(bytes.NewReader(archiveBytes)); err != nil {
		t.Fatalf("validate archive: %v", err)
	}
	r := tar.NewReader(bytes.NewReader(archiveBytes))
	var hardlink bool
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(h.Name, "/b.dat") && h.Typeflag == tar.TypeLink && strings.HasSuffix(h.Linkname, "/a.dat") {
			hardlink = true
		}
	}
	if !hardlink {
		t.Fatal("archive did not preserve the hardlink")
	}
	dest := filepath.Join(parent, "restore")
	if err := os.Mkdir(dest, 0755); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runInput(bytes.NewReader(archiveBytes), "extract", dest, strconv.Itoa(len(archiveBytes)))
	if code != 0 {
		t.Fatalf("extract exit %d: %s", code, stderr)
	}
	a := filepath.Join(dest, "source 雪", "a.dat")
	b := filepath.Join(dest, "source 雪", "b.dat")
	got, err := os.ReadFile(a)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("restored contents: bytes %d, %v", len(got), err)
	}
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Stat(b)
	if err != nil || !os.SameFile(ai, bi) {
		t.Fatalf("restored hardlink: %v", err)
	}
}

func TestExtractPreservesExternalSymlinkTarget(t *testing.T) {
	dest, external := t.TempDir(), t.TempDir()
	probe := filepath.Join(dest, "probe")
	if err := os.Symlink(external, probe); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	data := makeArchive(t, archiveItem{name: "link", kind: tar.TypeSymlink, link: external})
	_, stderr, code := runInput(bytes.NewReader(data), "extract", dest, strconv.Itoa(len(data)))
	if code != 0 {
		t.Fatalf("extract exit %d: %s", code, stderr)
	}
	target, err := os.Readlink(filepath.Join(dest, "link"))
	if err != nil || target != external {
		t.Fatalf("symlink target = %q, error %v; want %q", target, err, external)
	}
}

func TestArchiveValidationRejectsUnsafeStructures(t *testing.T) {
	cases := map[string][]archiveItem{
		"traversal":            {{name: "../escape", kind: tar.TypeReg}},
		"absolute":             {{name: "/escape", kind: tar.TypeReg}},
		"symlink parent":       {{name: "link", kind: tar.TypeSymlink, link: "/elsewhere"}, {name: "link/file", kind: tar.TypeReg}},
		"later symlink parent": {{name: "link/file", kind: tar.TypeReg}, {name: "link", kind: tar.TypeSymlink, link: "/elsewhere"}},
		"unknown hardlink":     {{name: "link", kind: tar.TypeLink, link: "outside"}},
		"duplicate":            {{name: "a", kind: tar.TypeReg}, {name: "a", kind: tar.TypeReg}},
	}
	if runtime.GOOS == "windows" {
		cases["reserved"] = []archiveItem{{name: "CON", kind: tar.TypeReg}}
		cases["reserved with extension"] = []archiveItem{{name: "CON.txt", kind: tar.TypeReg}}
		cases["trailing dot"] = []archiveItem{{name: "name.", kind: tar.TypeReg}}
		cases["case duplicate"] = []archiveItem{{name: "abc", kind: tar.TypeReg}, {name: "ABC", kind: tar.TypeReg}}
	}
	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			data := makeArchive(t, items...)
			if err := ValidateArchive(bytes.NewReader(data)); err == nil {
				t.Fatal("invalid archive accepted")
			}
			_, stderr, code := runInput(bytes.NewReader(data), "extract", t.TempDir(), strconv.Itoa(len(data)))
			if code == 0 || stderr == "" {
				t.Fatalf("invalid extraction exit %d: %s", code, stderr)
			}
		})
	}
	valid := makeArchive(t, archiveItem{name: "a", kind: tar.TypeReg, data: []byte("test")})
	if err := ValidateArchive(bytes.NewReader(valid[:len(valid)-512])); err == nil {
		t.Fatal("archive missing half its end marker was accepted")
	}
	if err := ValidateArchive(bytes.NewReader(append(valid, 1))); err == nil {
		t.Fatal("nonzero archive trailer was accepted")
	}
}

func TestArchiveValidatorAccountsForRetainedParents(t *testing.T) {
	validator := archiveValidator{entries: make(map[string]byte), parents: make(map[string]bool)}
	first := &tar.Header{Name: "a/b/one", Typeflag: tar.TypeReg}
	if _, err := validator.add(first); err != nil {
		t.Fatal(err)
	}
	want := len(first.Name) + len("a/b") + len("a")
	if validator.metadata != want {
		t.Fatalf("metadata bytes = %d, want %d", validator.metadata, want)
	}
	second := &tar.Header{Name: "a/b/two", Typeflag: tar.TypeReg}
	if _, err := validator.add(second); err != nil {
		t.Fatal(err)
	}
	if want += len(second.Name); validator.metadata != want {
		t.Fatalf("reused parents charged again: bytes = %d, want %d", validator.metadata, want)
	}

	nearLimit := archiveValidator{
		entries:  make(map[string]byte),
		parents:  make(map[string]bool),
		metadata: maxArchiveMetadata - len(first.Name) - len("a/b"),
	}
	if _, err := nearLimit.add(first); err == nil || !strings.Contains(err.Error(), "metadata exceeds") {
		t.Fatalf("parent prefix crossing the limit was accepted: %v", err)
	}
}

func TestExtractRejectsExistingSymlinkParentAndShortInput(t *testing.T) {
	dest, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(dest, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	data := makeArchive(t, archiveItem{name: "link/file", kind: tar.TypeReg, data: []byte("escape")})
	_, stderr, code := runInput(bytes.NewReader(data), "extract", dest, strconv.Itoa(len(data)))
	if code == 0 || !strings.Contains(stderr, "parent") {
		t.Fatalf("symlink parent exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(outside, "file")); !os.IsNotExist(err) {
		t.Fatalf("outside destination was changed: %v", err)
	}
	valid := makeArchive(t, archiveItem{name: "file", kind: tar.TypeReg, data: []byte("content")})
	_, stderr, code = runInput(bytes.NewReader(valid), "extract", t.TempDir(), strconv.Itoa(len(valid)+1))
	if code == 0 || !strings.Contains(stderr, "shorter") {
		t.Fatalf("short archive exit %d: %s", code, stderr)
	}
}
