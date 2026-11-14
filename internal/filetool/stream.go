package filetool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func fileType(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "symlink"
	case mode.Type() == os.ModeDir:
		return "directory"
	case mode.IsRegular():
		return "file"
	default:
		return "other"
	}
}

func stat(path string, stdout io.Writer) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %q: %w", path, err)
	}
	result := struct {
		Type  string    `json:"type"`
		Size  int64     `json:"size"`
		Mode  uint32    `json:"mode"`
		Mtime time.Time `json:"mtime"`
	}{fileType(info.Mode()), info.Size(), uint32(info.Mode().Perm()), info.ModTime().UTC()}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write file metadata: %w", err)
	}
	return nil
}

func read(path string, stdout io.Writer) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("read %q: path is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect open file %q: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fmt.Errorf("read %q: file changed while opening", path)
	}
	if _, err := io.Copy(stdout, file); err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	return nil
}

func parseSize(raw string) (int64, error) {
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("size must be a nonnegative decimal integer")
	}
	return size, nil
}

// write stages the exact input first, so a short or cancelled transfer cannot
// truncate an existing file. The final copy uses the existing file in place.
func write(path, rawSize, rawMode string, stdin io.Reader) (retErr error) {
	size, err := parseSize(rawSize)
	if err != nil {
		return err
	}
	mode, err := strconv.ParseUint(rawMode, 10, 32)
	if err != nil || mode > 0777 {
		return fmt.Errorf("mode must be a decimal permission value from 0 to 511")
	}
	if err := validateRemovePath(path); err != nil {
		return err
	}
	parent, name := filepath.Dir(path), filepath.Base(path)
	root, err := os.OpenRoot(parent)
	if err != nil {
		return fmt.Errorf("open parent directory %q: %w", parent, err)
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect write target %q: %w", path, err)
	}
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("write %q: existing target is not a regular file", path)
	}
	stage, err := os.CreateTemp(parent, ".ucd-write-*")
	if err != nil {
		return fmt.Errorf("stage write in %q: %w", parent, err)
	}
	defer func() {
		if err := stage.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close staged file: %w", err))
		}
		if err := os.Remove(stage.Name()); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove staged file: %w", err))
		}
	}()
	if _, err := io.CopyN(stage, stdin, size); err != nil {
		return fmt.Errorf("read %d input bytes: %w", size, err)
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind staged file: %w", err)
	}
	flags := os.O_WRONLY
	perm := os.FileMode(mode)
	if info == nil {
		flags |= os.O_CREATE | os.O_EXCL
		if perm == 0 {
			perm = 0644
		}
	}
	file, err := root.OpenFile(name, flags, perm)
	if err != nil {
		return fmt.Errorf("open write target %q: %w", path, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close write target %q: %w", path, err))
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect open target %q: %w", path, err)
	}
	if !opened.Mode().IsRegular() || (info != nil && !os.SameFile(info, opened)) {
		return fmt.Errorf("write %q: target changed while opening", path)
	}
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate %q: %w", path, err)
	}
	if _, err := io.Copy(file, stage); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	if mode != 0 {
		if err := file.Chmod(os.FileMode(mode)); err != nil {
			return fmt.Errorf("set mode on %q: %w", path, err)
		}
	}
	return nil
}
