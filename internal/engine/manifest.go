package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/skyri/ucd/internal/api"
)

type importedFile struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type importManifest struct {
	Files map[string]importedFile `json:"files"`
}

func newManifest(files []api.File) importManifest {
	m := importManifest{Files: make(map[string]importedFile, len(files))}
	for _, file := range files {
		sum := sha256.Sum256(file.Content)
		m.Files[filepath.ToSlash(filepath.Clean(file.Path))] = importedFile{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(file.Content))}
	}
	return m
}

func writeManifest(root *os.Root, files []api.File) error {
	data, err := json.MarshalIndent(newManifest(files), "", "  ")
	if err != nil {
		return err
	}
	return root.WriteFile(manifestName, append(data, '\n'), 0600)
}

func readManifest(root *os.Root) (importManifest, error) {
	if err := noSymlinks(root, manifestName); err != nil {
		return importManifest{}, err
	}
	f, err := root.Open(manifestName)
	if err != nil {
		return importManifest{}, fmt.Errorf("project import manifest is unavailable; cannot safely synchronize files: %w", err)
	}
	defer f.Close()
	var manifest importManifest
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&manifest); err != nil || manifest.Files == nil || len(manifest.Files) > maxProjectFiles {
		return importManifest{}, fmt.Errorf("invalid project import manifest; cannot safely synchronize files")
	}
	seen := map[string]bool{}
	for path, file := range manifest.Files {
		name, err := fileName(path)
		if err != nil || strings.EqualFold(name, manifestName) || strings.EqualFold(name, "compose.yaml") || seen[strings.ToLower(name)] || file.Size < 0 || len(file.SHA256) != 64 {
			return importManifest{}, fmt.Errorf("invalid project import manifest; cannot safely synchronize files")
		}
		if _, err := hex.DecodeString(file.SHA256); err != nil {
			return importManifest{}, fmt.Errorf("invalid project import manifest; cannot safely synchronize files")
		}
		seen[strings.ToLower(name)] = true
	}
	return manifest, nil
}

func matchesImportedFile(root *os.Root, name string, expected importedFile) (bool, error) {
	if err := noSymlinks(root, name); err != nil {
		return false, err
	}
	f, err := root.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != expected.Size {
		return false, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == expected.SHA256, nil
}
