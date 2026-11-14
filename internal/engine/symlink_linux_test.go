//go:build linux

package engine

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

func TestLinuxProjectSymlinkComponentsAreRejected(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Docker request %s", r.URL)
		w.WriteHeader(500)
	})
	yaml := "services:\n  web:\n    image: example\n"
	params := api.ComposeParams{Project: api.ProjectPart{Engine: "linux", Name: "demo"}, YAML: yaml, Files: []api.File{{Path: "config/data.txt", Content: []byte("original")}}}
	result, err := call(t, b, "compose.import", params)
	if err != nil {
		t.Fatal(err)
	}
	part := result.(api.ProjectPart)
	external := t.TempDir()
	externalFile := filepath.Join(external, "data.txt")
	if err := os.WriteFile(externalFile, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(part.Directory, "config", "data.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(part.Directory, "config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(part.Directory, "config")); err != nil {
		t.Fatal(err)
	}
	params.Project = part
	params.Replace = true
	params.Files[0].Content = []byte("overwrite")
	if _, err := call(t, b, "compose.import", params); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink replacement: %v", err)
	}
	if data, err := os.ReadFile(externalFile); err != nil || string(data) != "keep" {
		t.Fatalf("external file changed: %q %v", data, err)
	}

	if err := os.WriteFile(filepath.Join(external, "compose.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(b.dataRoot, "projects", "redirected")); err != nil {
		t.Fatal(err)
	}
	redirected := api.ProjectPart{Engine: "linux", Name: "redirected", Directory: filepath.Join(b.dataRoot, "projects", "redirected"), ComposeFile: filepath.Join(b.dataRoot, "projects", "redirected", "compose.yaml")}
	if _, err := call(t, b, "compose.read", api.ComposeParams{Project: redirected}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlink project read: %v", err)
	}
}
