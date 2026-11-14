package filetool

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type directoryMetadata struct {
	name string
	mode os.FileMode
	time time.Time
}

func extract(destination, rawSize string, stdin io.Reader) error {
	size, err := parseSize(rawSize)
	if err != nil {
		return err
	}
	info, err := os.Lstat(destination)
	if err != nil {
		return fmt.Errorf("inspect extraction destination %q: %w", destination, err)
	}
	if info.Mode().Type() != os.ModeDir {
		return fmt.Errorf("extraction destination %q is not a directory", destination)
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return fmt.Errorf("open extraction destination %q: %w", destination, err)
	}
	defer root.Close()
	limited := &io.LimitedReader{R: stdin, N: size}
	var directories []directoryMetadata
	err = scanArchive(limited, func(header *tar.Header, name string, content io.Reader) error {
		if err := ensureParents(root, name, true); err != nil {
			return err
		}
		local := filepath.FromSlash(name)
		info, err := root.Lstat(local)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect extraction target %q: %w", name, err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if info == nil {
				if err := root.Mkdir(local, 0755); err != nil {
					return fmt.Errorf("create directory %q: %w", name, err)
				}
			} else if info.Mode().Type() != os.ModeDir {
				return fmt.Errorf("extraction target %q is not a directory", name)
			}
			directories = append(directories, directoryMetadata{name, os.FileMode(header.Mode) & 0777, header.ModTime})
			return nil
		case tar.TypeReg, tar.TypeRegA:
			if info != nil && !info.Mode().IsRegular() {
				return fmt.Errorf("extraction target %q is not a regular file", name)
			}
			flags := os.O_WRONLY
			if info == nil {
				flags |= os.O_CREATE | os.O_EXCL
			}
			file, err := root.OpenFile(local, flags, os.FileMode(header.Mode)&0777)
			if err != nil {
				return fmt.Errorf("open extraction target %q: %w", name, err)
			}
			opened, opErr := file.Stat()
			if opErr == nil && (!opened.Mode().IsRegular() || (info != nil && !os.SameFile(info, opened))) {
				opErr = fmt.Errorf("target changed while opening")
			}
			if opErr == nil {
				opErr = file.Truncate(0)
			}
			if opErr == nil {
				_, opErr = io.CopyN(file, content, header.Size)
			}
			if opErr == nil {
				opErr = file.Chmod(os.FileMode(header.Mode) & 0777)
			}
			closeErr := file.Close()
			if err := errors.Join(opErr, closeErr); err != nil {
				return fmt.Errorf("extract file %q: %w", name, err)
			}
			return setTime(root, local, header.ModTime)
		case tar.TypeSymlink, tar.TypeLink:
			if info != nil {
				if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
					return fmt.Errorf("extraction target %q cannot be replaced by a link", name)
				}
				if err := root.Remove(local); err != nil {
					return fmt.Errorf("replace extraction target %q: %w", name, err)
				}
			}
			if header.Typeflag == tar.TypeSymlink {
				if err := root.Symlink(header.Linkname, local); err != nil {
					return fmt.Errorf("create symlink %q: %w", name, err)
				}
				return nil
			}
			target := path.Clean(header.Linkname)
			if err := ensureParents(root, target, false); err != nil {
				return err
			}
			linkInfo, err := root.Lstat(filepath.FromSlash(target))
			if err != nil || !linkInfo.Mode().IsRegular() {
				return fmt.Errorf("hardlink target %q is not an accessible regular file", target)
			}
			if err := root.Link(filepath.FromSlash(target), local); err != nil {
				return fmt.Errorf("create hardlink %q: %w", name, err)
			}
			return nil
		}
		return fmt.Errorf("unsupported archive entry %q", name)
	})
	if err != nil {
		return err
	}
	if limited.N != 0 {
		return fmt.Errorf("archive input is shorter than %d bytes: %w", size, io.ErrUnexpectedEOF)
	}
	for i := len(directories) - 1; i >= 0; i-- {
		dir := directories[i]
		local := filepath.FromSlash(dir.name)
		if err := root.Chmod(local, dir.mode); err != nil {
			return fmt.Errorf("set directory mode %q: %w", dir.name, err)
		}
		if err := setTime(root, local, dir.time); err != nil {
			return err
		}
	}
	return nil
}

func ensureParents(root *os.Root, name string, create bool) error {
	parts := strings.Split(name, "/")
	for i := 1; i < len(parts); i++ {
		parent := filepath.FromSlash(strings.Join(parts[:i], "/"))
		info, err := root.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := root.Mkdir(parent, 0755); err != nil {
				return fmt.Errorf("create extraction parent %q: %w", parent, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect extraction parent %q: %w", parent, err)
		}
		if info.Mode().Type() != os.ModeDir {
			return fmt.Errorf("extraction parent %q is not a directory or is a symlink", parent)
		}
	}
	return nil
}

func setTime(root *os.Root, name string, modified time.Time) error {
	if modified.IsZero() {
		return nil
	}
	if err := root.Chtimes(name, modified, modified); err != nil {
		return fmt.Errorf("set modification time on %q: %w", name, err)
	}
	return nil
}
