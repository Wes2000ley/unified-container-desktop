// Package filetool implements the native filesystem commands used inside containers.
package filetool

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type entry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Run executes a filesystem command and returns its process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if err := run(args, stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "ucd-filetool: %v\n", err)
		return 1
	}
	return 0
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: ucd-filetool list|stat|read|archive PATH | mkdir PATH | remove [--recursive] PATH | write PATH SIZE MODE | extract DEST SIZE")
	}

	command := args[0]
	recursive := command == "remove" && len(args) == 3 && args[1] == "--recursive"
	switch command {
	case "list", "stat", "read", "archive", "mkdir":
		if len(args) != 2 {
			return fmt.Errorf("%s requires PATH", command)
		}
	case "remove":
		if !recursive && len(args) != 2 {
			return fmt.Errorf("remove requires [--recursive] PATH")
		}
	case "write":
		if len(args) != 4 {
			return fmt.Errorf("write requires PATH SIZE MODE")
		}
	case "extract":
		if len(args) != 3 {
			return fmt.Errorf("extract requires DEST SIZE")
		}
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	path := args[1]
	if recursive {
		path = args[2]
	}
	if err := validatePath(path); err != nil {
		return err
	}

	switch command {
	case "list":
		return list(path, stdout)
	case "stat":
		return stat(path, stdout)
	case "read":
		return read(path, stdout)
	case "write":
		return write(path, args[2], args[3], stdin)
	case "archive":
		return archive(path, stdout)
	case "extract":
		return extract(path, args[2], stdin)
	case "mkdir":
		if err := os.Mkdir(path, 0755); err != nil {
			return fmt.Errorf("create directory %q: %w", path, err)
		}
		return nil
	default: // remove
		if err := validateRemovePath(path); err != nil {
			return err
		}
		// A trailing separator causes operating systems to follow a directory
		// symlink. Remove the link itself even if the caller included one.
		path = strings.TrimRightFunc(path, isPathSeparator)
		if _, err := os.Lstat(path); err != nil {
			return fmt.Errorf("inspect removal target %q: %w", path, err)
		}
		var err error
		if recursive {
			err = os.RemoveAll(path)
		} else {
			err = os.Remove(path)
		}
		if err != nil {
			return fmt.Errorf("remove %q: %w", path, err)
		}
		return nil
	}
}

func validatePath(path string) error {
	if strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) {
		return fmt.Errorf("an absolute filesystem path without NUL characters is required")
	}
	if runtime.GOOS == "windows" {
		native := strings.ReplaceAll(path, "/", `\`)
		if strings.HasPrefix(native, `\\?\`) || strings.HasPrefix(native, `\\.\`) || strings.HasPrefix(native, `\??\`) {
			return fmt.Errorf("Windows device namespace paths are not supported; use a drive or UNC path")
		}
	}
	return nil
}

func validateRemovePath(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	if strings.TrimLeftFunc(clean[len(volume):], isPathSeparator) == "" {
		return fmt.Errorf("cannot remove a filesystem root")
	}
	for _, component := range strings.FieldsFunc(path, isPathSeparator) {
		if component == "." || component == ".." {
			return fmt.Errorf("remove path must not contain . or .. components")
		}
	}
	return nil
}

func isPathSeparator(r rune) bool {
	return r == '/' || (runtime.GOOS == "windows" && r == '\\')
}

func list(path string, stdout io.Writer) error {
	items, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("list directory %q: %w", path, err)
	}
	entries := make([]entry, 0, len(items))
	for _, item := range items {
		entryPath := path
		if !isPathSeparator(rune(entryPath[len(entryPath)-1])) {
			entryPath += string(os.PathSeparator)
		}
		entryPath += item.Name()
		info, err := os.Lstat(entryPath)
		if err != nil {
			return fmt.Errorf("inspect directory entry %q: %w", entryPath, err)
		}
		entries = append(entries, entry{
			Name:     item.Name(),
			Path:     entryPath,
			Type:     fileType(info.Mode()),
			Size:     info.Size(),
			Modified: info.ModTime().UTC(),
		})
	}
	if err := json.NewEncoder(stdout).Encode(struct {
		Entries []entry `json:"entries"`
	}{Entries: entries}); err != nil {
		return fmt.Errorf("write directory listing: %w", err)
	}
	return nil
}
