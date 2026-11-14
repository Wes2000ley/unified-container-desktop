package filetool

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func command(args ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	code := Run(args, strings.NewReader(""), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func mustCommand(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, code := command(args...)
	if code != 0 || stderr != "" {
		t.Fatalf("command %q failed (exit %d): %s", args, code, stderr)
	}
	return stdout
}

func TestListIncludesNamesAndMetadata(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		".hidden":          "hidden",
		"snow 雪 space.txt": "contents",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "a directory"), 0755); err != nil {
		t.Fatal(err)
	}

	var listing struct {
		Entries []entry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(mustCommand(t, "list", dir)), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(listing.Entries), listing.Entries)
	}
	for _, got := range listing.Entries {
		if got.Path != filepath.Join(dir, got.Name) {
			t.Errorf("path for %q = %q", got.Name, got.Path)
		}
		if got.Modified.IsZero() || got.Modified.Location() != time.UTC {
			t.Errorf("modified for %q is not a UTC RFC3339 timestamp: %v", got.Name, got.Modified)
		}
		if got.Name == "a directory" {
			if got.Type != "directory" {
				t.Errorf("directory type = %q", got.Type)
			}
			continue
		}
		content, ok := files[got.Name]
		if !ok || got.Type != "file" || got.Size != int64(len(content)) {
			t.Errorf("unexpected file entry: %+v", got)
		}
		delete(files, got.Name)
	}
	if len(files) != 0 {
		t.Errorf("missing files: %v", files)
	}
}

func TestListEmptyDirectoryIsAnEmptyArray(t *testing.T) {
	stdout := mustCommand(t, "list", t.TempDir())
	if strings.TrimSpace(stdout) != `{"entries":[]}` {
		t.Fatalf("empty listing = %q", stdout)
	}
}

func TestMkdirAndRemove(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "new folder 雪")
	if output := mustCommand(t, "mkdir", dir); output != "" {
		t.Fatalf("mkdir wrote output %q", output)
	}
	file := filepath.Join(dir, ".file")
	if err := os.WriteFile(file, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := command("remove", dir); code == 0 || !strings.Contains(stderr, "remove") {
		t.Fatalf("nonrecursive removal of nonempty directory: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("failed removal changed contents: %v", err)
	}
	if output := mustCommand(t, "remove", "--recursive", dir); output != "" {
		t.Fatalf("remove wrote output %q", output)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recursive removal did not remove directory: %v", err)
	}
	if _, stderr, code := command("remove", "--recursive", dir); code == 0 || !strings.Contains(stderr, "inspect removal target") {
		t.Fatalf("missing target: exit %d, stderr %q", code, stderr)
	}

	emptyDir := filepath.Join(parent, "empty")
	mustCommand(t, "mkdir", emptyDir)
	mustCommand(t, "remove", emptyDir)
	file = filepath.Join(parent, "regular file")
	if err := os.WriteFile(file, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	mustCommand(t, "remove", file)
	if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file remains after removal: %v", err)
	}
}

func TestSymlinksAreListedAndRemovedWithoutFollowing(t *testing.T) {
	parent, target := t.TempDir(), t.TempDir()
	protected := filepath.Join(target, "keep.txt")
	if err := os.WriteFile(protected, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symbolic links unavailable on this host: %v", err)
	}
	var listing struct {
		Entries []entry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(mustCommand(t, "list", parent)), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Type != "symlink" {
		t.Fatalf("symlink listing = %+v", listing.Entries)
	}
	mustCommand(t, "remove", "--recursive", link+string(os.PathSeparator))
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("link remains: %v", err)
	}
	if content, err := os.ReadFile(protected); err != nil || string(content) != "keep" {
		t.Fatalf("symlink target changed: %q, %v", content, err)
	}

	tree := filepath.Join(parent, "tree")
	if err := os.Mkdir(tree, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(tree, "nested link")); err != nil {
		t.Fatal(err)
	}
	mustCommand(t, "remove", "--recursive", tree)
	if content, err := os.ReadFile(protected); err != nil || string(content) != "keep" {
		t.Fatalf("recursive removal followed nested symlink: %q, %v", content, err)
	}
}

func TestRemovePathProtectsRootsAndAmbiguousComponents(t *testing.T) {
	root := string(os.PathSeparator)
	if runtime.GOOS == "windows" {
		root = filepath.VolumeName(t.TempDir()) + root
	}
	for _, path := range []string{root, root + ".", root + "missing" + string(os.PathSeparator) + ".."} {
		if err := validateRemovePath(path); err == nil {
			t.Errorf("root path accepted: %q", path)
		}
	}
	for _, path := range []string{
		root + "dir" + string(os.PathSeparator) + "." + string(os.PathSeparator) + "child",
		root + "dir" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "child",
	} {
		if err := validateRemovePath(path); err == nil {
			t.Errorf("ambiguous remove path accepted: %q", path)
		}
	}
	if err := validateRemovePath(root + ".hidden"); err != nil {
		t.Fatalf("dotfile path rejected: %v", err)
	}
	if runtime.GOOS == "windows" {
		for _, path := range []string{`\\server\share`, `\\server\share\`, `//server/share/`} {
			if err := validatePath(path); err != nil {
				t.Errorf("UNC path %q rejected: %v", path, err)
			} else if err := validateRemovePath(path); err == nil {
				t.Errorf("UNC root path accepted for removal: %q", path)
			}
		}
		if err := validateRemovePath(`\\server\share\child`); err != nil {
			t.Errorf("UNC child rejected: %v", err)
		}
		for _, path := range []string{`\\?\C:\`, `\\?\UNC\server\share`, `\\.\C:\`} {
			if err := validatePath(path); err == nil {
				t.Errorf("device namespace path accepted: %q", path)
			}
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestErrorsAreReported(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		nil,
		{"unknown", root},
		{"list", "relative"},
		{"list", filepath.Join(root, "missing")},
		{"mkdir", filepath.Join(root, "missing", "child")},
		{"remove", "--recursive"},
		{"remove", "--recursive", root, "extra"},
	} {
		_, stderr, code := command(args...)
		if code == 0 || !strings.HasPrefix(stderr, "ucd-filetool: ") {
			t.Errorf("args %q: exit %d, stderr %q", args, code, stderr)
		}
	}
	var stderr bytes.Buffer
	if code := Run([]string{"list", root}, strings.NewReader(""), failingWriter{}, &stderr); code == 0 || !strings.Contains(stderr.String(), "write directory listing") {
		t.Errorf("output failure: exit %d, stderr %q", code, stderr.String())
	}
}
