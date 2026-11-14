package filetool

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

const maxArchiveEntries = 250000
const maxArchiveMetadata = 64 << 20

// ValidateArchive checks the same archive structure accepted by extract. The
// reader must end after the archive's zero-filled trailer.
func ValidateArchive(src io.Reader) error {
	return scanArchive(src, nil)
}

type archiveValidator struct {
	entries  map[string]byte
	parents  map[string]bool
	metadata int
}

func archiveKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

func archiveName(name string) (string, error) {
	name = strings.TrimSuffix(name, "/")
	if len(name) > 4096 || !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00") {
		return "", fmt.Errorf("archive contains unsafe path %q", name)
	}
	if _, err := filepath.Localize(name); err != nil {
		return "", fmt.Errorf("archive path %q is not supported on this filesystem: %w", name, err)
	}
	if runtime.GOOS == "windows" {
		for _, part := range strings.Split(name, "/") {
			if strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") || strings.ContainsAny(part, `<>"|?*`) || windowsReservedName(part) {
				return "", fmt.Errorf("archive path %q is not a valid Windows name", name)
			}
			for _, r := range part {
				if r < 32 {
					return "", fmt.Errorf("archive path %q is not a valid Windows name", name)
				}
			}
		}
	}
	return name, nil
}

func windowsReservedName(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT") {
		suffix := stem[3:]
		return len(suffix) == 1 && suffix[0] >= '1' && suffix[0] <= '9' || suffix == "¹" || suffix == "²" || suffix == "³"
	}
	return false
}

func (v *archiveValidator) add(header *tar.Header) (string, error) {
	name, err := archiveName(header.Name)
	if err != nil {
		return "", err
	}
	if len(v.entries) >= maxArchiveEntries {
		return "", fmt.Errorf("archive contains more than %d entries", maxArchiveEntries)
	}
	v.metadata += len(name) + len(header.Linkname)
	if v.metadata > maxArchiveMetadata {
		return "", fmt.Errorf("archive path metadata exceeds 64 MiB")
	}
	for key := range header.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse") || key == "SCHILY.realsize" {
			return "", fmt.Errorf("archive contains unsupported sparse metadata")
		}
	}
	switch header.Typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
	case tar.TypeSymlink:
		if header.Linkname == "" || strings.ContainsRune(header.Linkname, 0) {
			return "", fmt.Errorf("archive contains an invalid symlink target")
		}
	case tar.TypeLink:
		link := header.Linkname
		if link == "" || strings.HasPrefix(link, "/") || strings.ContainsAny(link, "\\:\x00") {
			return "", fmt.Errorf("archive contains an unsafe hardlink")
		}
		link = path.Clean(link)
		if link == ".." || strings.HasPrefix(link, "../") {
			return "", fmt.Errorf("archive hardlink escapes its destination")
		}
		kind, ok := v.entries[archiveKey(link)]
		if !ok || (kind != tar.TypeReg && kind != tar.TypeRegA && kind != tar.TypeLink) {
			return "", fmt.Errorf("archive hardlink must target a preceding regular file or hardlink")
		}
	default:
		return "", fmt.Errorf("archive entry %q has unsupported type %d", name, header.Typeflag)
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA && header.Size != 0 {
		return "", fmt.Errorf("archive non-file entry %q contains file data", name)
	}
	key := archiveKey(name)
	if _, exists := v.entries[key]; exists {
		return "", fmt.Errorf("archive contains duplicate path %q", name)
	}
	if header.Typeflag != tar.TypeDir && v.parents[key] {
		return "", fmt.Errorf("archive contains an entry beneath non-directory %q", name)
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		parentKey := archiveKey(parent)
		if kind, exists := v.entries[parentKey]; exists && kind != tar.TypeDir {
			return "", fmt.Errorf("archive contains an entry beneath non-directory %q", parent)
		}
		if !v.parents[parentKey] {
			v.metadata += len(parentKey)
			if v.metadata > maxArchiveMetadata {
				return "", fmt.Errorf("archive path metadata exceeds 64 MiB")
			}
			v.parents[parentKey] = true
		}
	}
	v.entries[key] = header.Typeflag
	return name, nil
}

type countingReader struct {
	io.Reader
	count int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.count += int64(n)
	return n, err
}

// scanArchive consumes and validates the complete tar, including its trailer.
// handle must consume regular-file contents it needs before returning.
func scanArchive(src io.Reader, handle func(*tar.Header, string, io.Reader) error) error {
	tracked := &countingReader{Reader: src}
	reader := tar.NewReader(tracked)
	validator := archiveValidator{entries: make(map[string]byte), parents: make(map[string]bool)}
	var padding int64
	for {
		before := tracked.count
		header, err := reader.Next()
		if err == io.EOF {
			if tracked.count-before < padding+1024 {
				return fmt.Errorf("archive is missing its complete end marker")
			}
			var buf [32 << 10]byte
			for {
				n, err := tracked.Read(buf[:])
				for _, b := range buf[:n] {
					if b != 0 {
						return fmt.Errorf("archive contains data after its end marker")
					}
				}
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return fmt.Errorf("read archive trailer: %w", err)
				}
			}
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		name, err := validator.add(header)
		if err != nil {
			return err
		}
		if handle != nil {
			if err := handle(header, name, reader); err != nil {
				return err
			}
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return fmt.Errorf("read archive entry %q: %w", name, err)
		}
		padding = (512 - header.Size%512) % 512
	}
}

func archive(source string, stdout io.Writer) error {
	clean := filepath.Clean(source)
	base := filepath.Base(clean)
	if base == "." || base == string(os.PathSeparator) {
		base = "files"
	}
	writer := tar.NewWriter(stdout)
	hardlinks := make(map[fileIdentity]string)
	err := filepath.WalkDir(clean, func(current string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk archive path %q: %w", current, walkErr)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect archive path %q: %w", current, err)
		}
		kind := fileType(info.Mode())
		if kind == "other" {
			return fmt.Errorf("archive path %q has an unsupported file type", current)
		}
		var target string
		if kind == "symlink" {
			target, err = os.Readlink(current)
			if err != nil {
				return fmt.Errorf("read symlink %q: %w", current, err)
			}
		}
		header, err := tar.FileInfoHeader(info, target)
		if err != nil {
			return fmt.Errorf("prepare archive entry %q: %w", current, err)
		}
		rel, err := filepath.Rel(clean, current)
		if err != nil {
			return fmt.Errorf("name archive entry %q: %w", current, err)
		}
		header.Name = base
		if rel != "." {
			header.Name = path.Join(base, filepath.ToSlash(rel))
		}
		if kind == "directory" {
			header.Name += "/"
		}
		if kind == "file" {
			identity, linked, err := hardlinkIdentity(current, info)
			if err != nil {
				return fmt.Errorf("inspect file identity %q: %w", current, err)
			}
			if linked {
				if previous, ok := hardlinks[identity]; ok {
					header.Typeflag = tar.TypeLink
					header.Linkname = previous
					header.Size = 0
				} else {
					hardlinks[identity] = header.Name
				}
			}
		}
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write archive entry %q: %w", current, err)
		}
		if kind != "file" || header.Typeflag == tar.TypeLink {
			return nil
		}
		file, err := os.Open(current)
		if err != nil {
			return fmt.Errorf("open archive file %q: %w", current, err)
		}
		opened, statErr := file.Stat()
		if statErr == nil && (!opened.Mode().IsRegular() || !os.SameFile(info, opened)) {
			statErr = fmt.Errorf("file changed while opening")
		}
		if statErr == nil {
			_, statErr = io.CopyN(writer, file, header.Size)
		}
		closeErr := file.Close()
		if err := errors.Join(statErr, closeErr); err != nil {
			return fmt.Errorf("archive file %q: %w", current, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish archive: %w", err)
	}
	return nil
}
