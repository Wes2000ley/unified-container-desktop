package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/skyri/ucd/internal/api"
)

type replacedFile struct {
	name        string
	hadOriginal bool
}

// replaceProject synchronizes tracked source files without deleting untracked data.
// Backups allow rollback if validation or a rename fails.
func (b *Backend) replaceProject(ctx context.Context, projects *os.Root, p api.ComposeParams) (any, error) {
	if err := noSymlinks(projects, p.Project.Name); err != nil {
		return nil, err
	}
	root, err := projects.OpenRoot(p.Project.Name)
	if err != nil {
		return nil, fmt.Errorf("open existing project: %w", err)
	}
	defer root.Close()
	if err := noSymlinks(root, "compose.yaml"); err != nil {
		return nil, err
	}
	previous, err := readManifest(root)
	if err != nil {
		return nil, err
	}
	tmp, err := randomName()
	if err != nil {
		return nil, err
	}
	if err := root.Mkdir(tmp, 0700); err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = root.RemoveAll(tmp)
		}
	}()
	if err := root.Mkdir(filepath.Join(tmp, "incoming"), 0700); err != nil {
		return nil, err
	}
	if err := root.Mkdir(filepath.Join(tmp, "backup"), 0700); err != nil {
		return nil, err
	}
	incoming, err := root.OpenRoot(filepath.Join(tmp, "incoming"))
	if err != nil {
		return nil, err
	}
	if err := writeFiles(incoming, p.YAML, p.Files); err != nil {
		_ = incoming.Close()
		return nil, err
	}
	if err := writeManifest(incoming, p.Files); err != nil {
		_ = incoming.Close()
		return nil, err
	}
	if err := incoming.Close(); err != nil {
		return nil, err
	}
	current := newManifest(p.Files)
	key := func(name string) string {
		if runtime.GOOS == "windows" {
			return strings.ToLower(name)
		}
		return name
	}
	newPaths := make(map[string]bool, len(current.Files))
	for name := range current.Files {
		newPaths[key(name)] = true
	}
	oldPaths := make(map[string]importedFile, len(previous.Files))
	for name, file := range previous.Files {
		oldPaths[key(name)] = file
	}
	for name := range current.Files {
		localName := filepath.FromSlash(name)
		if err := noSymlinks(root, localName); err != nil {
			return nil, err
		}
		_, err := root.Lstat(localName)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		expected, tracked := oldPaths[key(name)]
		if !tracked {
			return nil, fmt.Errorf("project file %q is untracked; move it before importing a source file with the same name", name)
		}
		match, err := matchesImportedFile(root, localName, expected)
		if err != nil {
			return nil, err
		}
		if !match {
			return nil, fmt.Errorf("tracked file %q has changed since import; preserve or move it before replacing it", name)
		}
	}
	var stale []string
	for name, expected := range previous.Files {
		if newPaths[key(name)] {
			continue
		}
		match, err := matchesImportedFile(root, filepath.FromSlash(name), expected)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !match {
			return nil, fmt.Errorf("tracked file %q has changed since import; preserve or move it before removing it from the source", name)
		}
		stale = append(stale, name)
	}
	sort.Strings(stale)

	changed := make([]replacedFile, 0, len(p.Files))
	rollback := func(cause error) error {
		var rollbackErr error
		for i := len(changed) - 1; i >= 0; i-- {
			file := changed[i]
			if err := root.Remove(file.name); err != nil && !os.IsNotExist(err) {
				rollbackErr = errors.Join(rollbackErr, err)
			}
			if file.hadOriginal {
				if err := root.Rename(filepath.Join(tmp, "backup", file.name), file.name); err != nil {
					rollbackErr = errors.Join(rollbackErr, err)
				}
			}
		}
		if rollbackErr != nil {
			cleanup = false
			return errors.Join(cause, fmt.Errorf("rollback failed; backups remain in %s: %w", filepath.Join(root.Name(), tmp, "backup"), rollbackErr))
		}
		return cause
	}
	for _, source := range stale {
		name := filepath.FromSlash(source)
		backup := filepath.Join(tmp, "backup", name)
		if err := root.MkdirAll(filepath.Dir(backup), 0700); err != nil {
			return nil, rollback(err)
		}
		if err := root.Rename(name, backup); err != nil {
			return nil, rollback(err)
		}
		changed = append(changed, replacedFile{name: name, hadOriginal: true})
		match, err := matchesImportedFile(root, backup, previous.Files[source])
		if err != nil {
			return nil, rollback(err)
		}
		if !match {
			return nil, rollback(fmt.Errorf("tracked file %q changed during synchronization", source))
		}
	}
	for _, file := range p.Files {
		name, err := fileName(file.Path)
		if err != nil {
			return nil, rollback(err)
		}
		if err := noSymlinks(root, name); err != nil {
			return nil, rollback(err)
		}
		if err := createProjectDirs(root, filepath.Dir(name)); err != nil {
			return nil, rollback(err)
		}
		info, err := root.Lstat(name)
		hadOriginal := err == nil
		if err != nil && !os.IsNotExist(err) {
			return nil, rollback(err)
		}
		if hadOriginal {
			if !info.Mode().IsRegular() {
				return nil, rollback(fmt.Errorf("existing project path %q is not a regular file", name))
			}
			if err := root.MkdirAll(filepath.Join(tmp, "backup", filepath.Dir(name)), 0700); err != nil {
				return nil, rollback(err)
			}
			if err := root.Rename(name, filepath.Join(tmp, "backup", name)); err != nil {
				return nil, rollback(err)
			}
		}
		changed = append(changed, replacedFile{name: name, hadOriginal: hadOriginal})
		if hadOriginal {
			expected := oldPaths[key(filepath.ToSlash(name))]
			match, err := matchesImportedFile(root, filepath.Join(tmp, "backup", name), expected)
			if err != nil {
				return nil, rollback(err)
			}
			if !match {
				return nil, rollback(fmt.Errorf("tracked file %q changed during synchronization", name))
			}
		}
		if err := root.Rename(filepath.Join(tmp, "incoming", name), name); err != nil {
			return nil, rollback(err)
		}
	}
	part := api.ProjectPart{Engine: b.id, Name: p.Project.Name, Directory: filepath.Join(projects.Name(), p.Project.Name)}
	part.ComposeFile = filepath.Join(part.Directory, tmp, "incoming", "compose.yaml")
	project, err := b.loadStagedProject(ctx, part)
	if err == nil {
		_, err = projectPorts(project)
	}
	if err != nil {
		return nil, rollback(err)
	}
	for _, name := range []string{"compose.yaml", manifestName} {
		info, err := root.Lstat(name)
		if err != nil {
			return nil, rollback(err)
		}
		if !info.Mode().IsRegular() || info.Size() > maxYAMLBytes {
			return nil, rollback(fmt.Errorf("existing %s is not a valid regular file", name))
		}
		data, err := root.ReadFile(name)
		if err != nil {
			return nil, rollback(err)
		}
		if err := root.WriteFile(filepath.Join(tmp, "backup", name), data, 0600); err != nil {
			return nil, rollback(err)
		}
		if err := root.Rename(filepath.Join(tmp, "incoming", name), name); err != nil {
			return nil, rollback(fmt.Errorf("replace %s: %w", name, err))
		}
		changed = append(changed, replacedFile{name: name, hadOriginal: true})
	}
	for _, file := range changed {
		expected, tracked := oldPaths[key(filepath.ToSlash(file.name))]
		if !tracked || !file.hadOriginal {
			continue
		}
		match, err := matchesImportedFile(root, filepath.Join(tmp, "backup", file.name), expected)
		if err != nil {
			return nil, rollback(err)
		}
		if !match {
			return nil, rollback(fmt.Errorf("tracked file %q changed during synchronization", file.name))
		}
	}
	part.ComposeFile = filepath.Join(part.Directory, "compose.yaml")
	return part, nil
}
