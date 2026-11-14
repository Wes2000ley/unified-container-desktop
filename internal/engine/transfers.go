package engine

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/filetool"
)

const (
	transferChunkSize   = 256 << 10
	transferLimit       = 64
	transferIdleTime    = 10 * time.Minute
	volumeEntryLimit    = 250000
	volumeMetadataLimit = 64 << 20
)

// transferSession owns one Docker stream or private restore spool. ioMu
// serializes client I/O; abort may close the stream concurrently to unblock it.
type transferSession struct {
	ioMu       sync.Mutex
	stopOnce   sync.Once
	cleanupErr error
	ctx        context.Context
	cancel     context.CancelFunc
	reader     io.ReadCloser
	writer     io.WriteCloser
	spool      *os.File
	done       chan struct{}
	result     any
	err        error
	pendingErr error
	finished   bool
	volume     string
	header     *tar.Header
	tarWriter  *tar.Writer
	started    bool
	bytes      int64
	timer      *time.Timer // protected by Backend.transferMu
	lastActive time.Time   // protected by Backend.transferMu
	active     int         // protected by Backend.transferMu
}

type transferReader struct {
	io.Reader
	io.Closer
}

type fileTransferReady struct {
	name   string
	size   *int64
	header *tar.Header
	err    error
}

type volumeArchiveReader struct {
	io.Reader
	bytes int64
	tail  [1024]byte
}

func (r *volumeArchiveReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += int64(n)
	if n >= len(r.tail) {
		copy(r.tail[:], p[n-len(r.tail):n])
	} else if n > 0 {
		copy(r.tail[:], r.tail[n:])
		copy(r.tail[len(r.tail)-n:], p[:n])
	}
	return n, err
}

func (b *Backend) openTransfer(ctx context.Context, p api.TransferOpenParams) (result api.TransferOpenResult, retErr error) {
	streamCtx, cancel, detach := newStreamContext(ctx)
	defer detach()
	s := &transferSession{ctx: streamCtx, cancel: cancel, volume: p.Name}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, s.abort())
			s.wait()
		}
	}()
	switch p.Operation {
	case "image.save":
		if len(p.References) == 0 {
			return result, api.Fail("invalid_argument", "at least one image reference is required")
		}
		for _, ref := range p.References {
			if strings.TrimSpace(ref) == "" {
				return result, api.Fail("invalid_argument", "image references cannot be empty")
			}
		}
		var err error
		s.reader, err = b.docker.ImageSave(streamCtx, p.References)
		if err != nil {
			return result, fmt.Errorf("save image: %w", err)
		}
		result.Name = "images.tar"
	case "container.export":
		if p.ID == "" {
			return result, api.Fail("invalid_argument", "container ID is required")
		}
		var err error
		s.reader, err = b.docker.ContainerExport(streamCtx, p.ID, client.ContainerExportOptions{})
		if err != nil {
			return result, fmt.Errorf("export container: %w", err)
		}
		result.Name = "container.tar"
	case "image.load", "image.import", "image.build":
		reader, writer := io.Pipe()
		s.writer, s.done = writer, make(chan struct{})
		go func() {
			defer close(s.done)
			var output api.ResourceOutput
			var err error
			if p.Operation == "image.build" {
				output, err = b.streamImageBuild(streamCtx, reader, p)
			} else if p.Operation == "image.load" {
				var stream io.ReadCloser
				stream, err = b.docker.ImageLoad(streamCtx, reader)
				if err == nil {
					output, err = b.resourceProgress(stream, "load image", p.OperationID)
					_ = stream.Close()
				}
			} else {
				var stream io.ReadCloser
				stream, err = b.docker.ImageImport(streamCtx, client.ImageImportSource{Source: reader, SourceName: "-"}, p.Reference, client.ImageImportOptions{})
				if err == nil {
					output, err = b.resourceProgress(stream, "import image", p.OperationID)
					_ = stream.Close()
				}
			}
			if err != nil {
				err = fmt.Errorf("%s: %w", strings.ReplaceAll(p.Operation, ".", " "), err)
			}
			s.result, s.err = output, err
			_ = reader.CloseWithError(err)
		}()
		result.Name = "images.tar"
		if p.Operation == "image.build" {
			result.Name = "context.tar"
		}
	case "volume.backup", "volume.restore", "volume.files.upload", "volume.files.download", "volume.files.archive":
		if p.Name == "" {
			return result, api.Fail("invalid_argument", "volume name is required")
		}
		if p.Operation == "volume.restore" && !p.Confirmed {
			return result, api.Fail("confirmation_required", "confirm before replacing volume contents")
		}
		if _, err := b.docker.VolumeInspect(streamCtx, p.Name, client.VolumeInspectOptions{}); err != nil {
			return result, fmt.Errorf("inspect volume: %w", err)
		}
		switch p.Operation {
		case "volume.backup":
			reader, writer := io.Pipe()
			s.reader, s.done = reader, make(chan struct{})
			go func() {
				defer close(s.done)
				s.err = b.streamVolumeBackup(streamCtx, p.Name, writer)
				_ = writer.CloseWithError(s.err)
			}()
			result.Name = p.Name + ".tar"
		case "volume.restore":
			file, err := os.CreateTemp(b.dataRoot, ".ucd-volume-restore-*")
			if err != nil {
				return result, fmt.Errorf("create private volume restore spool: %w", err)
			}
			s.spool = file
			result.Name = p.Name + ".tar"
		default:
			ready, err := b.openVolumeFileTransfer(s, p)
			if err != nil {
				return result, err
			}
			result.Name, result.Size, s.header = ready.name, ready.size, ready.header
		}
	case "container.files.download", "container.files.archive":
		if p.ID == "" {
			return result, api.Fail("invalid_argument", "container ID is required")
		}
		var err error
		p.Path, err = containerPath(b.id, p.Path)
		if err != nil {
			return result, err
		}
		s.reader, result.Name, result.Size, err = b.openContainerDownload(streamCtx, p.ID, p.Path, p.Operation == "container.files.archive")
		if err != nil {
			return result, err
		}
	case "container.files.upload":
		if p.ID == "" {
			return result, api.Fail("invalid_argument", "container ID is required")
		}
		var err error
		p.Path, err = containerPath(b.id, p.Path)
		if err != nil {
			return result, err
		}
		if p.Size == nil || *p.Size < 0 {
			return result, api.Fail("invalid_argument", "a nonnegative file size is required")
		}
		if b.id == "windows" {
			if containerRoot(b.id, p.Path) {
				return result, api.Fail("invalid_argument", "file path must include a name")
			}
			writer, done, err := b.openFileToolWrite(streamCtx, p.ID, fileToolWriteArgs(p.Path, *p.Size, p.Mode))
			if err != nil {
				return result, err
			}
			s.writer, s.header, s.done = writer, &tar.Header{Size: *p.Size}, make(chan struct{})
			go func() {
				defer close(s.done)
				s.err = <-done
			}()
			result.Name = path.Base(p.Path)
			break
		}
		header, parent, err := b.prepareContainerUpload(streamCtx, p.ID, p.Path, *p.Size, p.Mode)
		if err != nil {
			return result, err
		}
		s.header = header
		reader, writer := io.Pipe()
		s.writer, s.tarWriter, s.done = writer, tar.NewWriter(writer), make(chan struct{})
		go func() {
			defer close(s.done)
			_, s.err = b.docker.CopyToContainer(streamCtx, p.ID, client.CopyToContainerOptions{DestinationPath: parent, Content: reader, CopyUIDGID: b.id == "linux"})
			if s.err != nil {
				s.err = fmt.Errorf("write container file: %w", s.err)
			}
			_ = reader.CloseWithError(s.err)
		}()
		result.Name = path.Base(p.Path)
	default:
		return result, api.Fail("invalid_argument", "unknown transfer operation")
	}
	id, err := b.registerTransfer(s)
	if err != nil {
		return result, err
	}
	// A canceled opening call must not leave a detached stream behind.
	if err := ctx.Err(); err != nil {
		b.removeTransfer(id)
		return result, err
	}
	result.SessionID = id
	return result, nil
}

func (b *Backend) openContainerDownload(ctx context.Context, id, source string, archive bool) (io.ReadCloser, string, *int64, error) {
	name := path.Base(source)
	if name == "/" || name == "." || strings.HasSuffix(name, ":") {
		name = "files"
	}
	if b.id == "windows" {
		operation := "read"
		var size *int64
		if archive {
			operation = "archive"
			name += ".tar"
		} else {
			data, err := b.runFileTool(ctx, id, []string{"stat", source})
			if err != nil {
				return nil, "", nil, fmt.Errorf("inspect container file: %w", err)
			}
			var info struct {
				Type string `json:"type"`
				Size int64  `json:"size"`
			}
			if err := json.Unmarshal(data, &info); err != nil {
				return nil, "", nil, fmt.Errorf("decode container file metadata: %w", err)
			}
			if info.Type != "file" || info.Size < 0 {
				return nil, "", nil, api.Fail("invalid_argument", "path is not a regular file; use archive for directories")
			}
			size = &info.Size
		}
		reader, _, err := b.openFileToolRead(ctx, id, []string{operation, source})
		if err != nil {
			return nil, "", nil, fmt.Errorf("open container file transfer: %w", err)
		}
		return reader, name, size, nil
	}
	result, err := b.docker.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: source})
	if err != nil {
		return nil, "", nil, fmt.Errorf("copy container path: %w", err)
	}
	if archive {
		return result.Content, name + ".tar", nil, nil
	}
	if !result.Stat.Mode.IsRegular() {
		_ = result.Content.Close()
		return nil, "", nil, api.Fail("invalid_argument", "path is not a regular file; use archive for directories")
	}
	reader := tar.NewReader(result.Content)
	header, err := reader.Next()
	if err != nil {
		_ = result.Content.Close()
		return nil, "", nil, fmt.Errorf("read file archive: %w", err)
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
		_ = result.Content.Close()
		return nil, "", nil, fmt.Errorf("container archive does not contain a regular file")
	}
	size := header.Size
	return transferReader{Reader: reader, Closer: result.Content}, name, &size, nil
}

func fileToolWriteArgs(destination string, size int64, mode uint32) []string {
	return []string{"write", destination, strconv.FormatInt(size, 10), strconv.FormatUint(uint64(mode&0777), 10)}
}

func (b *Backend) prepareContainerUpload(ctx context.Context, id, destination string, size int64, mode uint32) (*tar.Header, string, error) {
	if containerRoot(b.id, destination) {
		return nil, "", api.Fail("invalid_argument", "file path must include a name")
	}
	header := &tar.Header{Name: path.Base(destination), Mode: 0644, Size: size, Typeflag: tar.TypeReg}
	existing, err := b.docker.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: destination})
	if err == nil {
		if !existing.Stat.Mode.IsRegular() {
			_ = existing.Content.Close()
			return nil, "", api.Fail("invalid_argument", "destination exists and is not a regular file")
		}
		previous, readErr := tar.NewReader(existing.Content).Next()
		_ = existing.Content.Close()
		if readErr != nil {
			return nil, "", fmt.Errorf("read existing file metadata: %w", readErr)
		}
		if previous.Typeflag != tar.TypeReg && previous.Typeflag != tar.TypeRegA {
			return nil, "", fmt.Errorf("existing file archive is not a regular file")
		}
		header.Mode, header.Uid, header.Gid = previous.Mode, previous.Uid, previous.Gid
		header.Uname, header.Gname = previous.Uname, previous.Gname
	} else if !errdefs.IsNotFound(err) {
		return nil, "", fmt.Errorf("inspect existing file: %w", err)
	}
	if mode != 0 {
		header.Mode = int64(mode & 0777)
	}
	parent := path.Dir(destination)
	if b.id == "windows" && len(parent) == 2 && parent[1] == ':' {
		parent += "/"
	}
	return header, parent, nil
}

func (b *Backend) openVolumeFileTransfer(s *transferSession, p api.TransferOpenParams) (fileTransferReady, error) {
	upload := p.Operation == "volume.files.upload"
	if upload && (p.Size == nil || *p.Size < 0) {
		return fileTransferReady{}, api.Fail("invalid_argument", "a nonnegative file size is required")
	}
	if _, err := volumePath("/ucd", p.Path); err != nil {
		return fileTransferReady{}, err
	}
	reader, writer := io.Pipe()
	s.done = make(chan struct{})
	if upload {
		s.writer = writer
		if b.id != "windows" {
			s.tarWriter = tar.NewWriter(writer)
		}
	} else {
		s.reader = reader
	}
	ready := make(chan fileTransferReady, 1)
	go func() {
		defer close(s.done)
		announced := false
		_, err := b.withVolumeContainer(s.ctx, p.Name, func(id, root string) (any, error) {
			mapped, err := volumePath(root, p.Path)
			if err != nil {
				return nil, err
			}
			mapped, err = containerPath(b.id, mapped)
			if err != nil {
				return nil, err
			}
			root, err = containerPath(b.id, root)
			if err != nil {
				return nil, err
			}
			if upload {
				if mapped == root {
					return nil, api.Fail("invalid_argument", "file path must include a name")
				}
				if b.id == "windows" {
					destination, done, err := b.openFileToolWrite(s.ctx, id, fileToolWriteArgs(mapped, *p.Size, p.Mode))
					if err != nil {
						return nil, err
					}
					ready <- fileTransferReady{name: path.Base(mapped), header: &tar.Header{Size: *p.Size}}
					announced = true
					_, copyErr := io.Copy(destination, reader)
					closeErr := destination.Close()
					return nil, errors.Join(copyErr, closeErr, <-done)
				}
				header, parent, err := b.prepareContainerUpload(s.ctx, id, mapped, *p.Size, p.Mode)
				if err != nil {
					return nil, err
				}
				ready <- fileTransferReady{name: path.Base(mapped), header: header}
				announced = true
				_, err = b.docker.CopyToContainer(s.ctx, id, client.CopyToContainerOptions{DestinationPath: parent, Content: reader, CopyUIDGID: b.id == "linux"})
				if err != nil {
					return nil, fmt.Errorf("write volume file: %w", err)
				}
				return nil, nil
			}
			source, name, size, err := b.openContainerDownload(s.ctx, id, mapped, p.Operation == "volume.files.archive")
			if err != nil {
				return nil, err
			}
			defer source.Close()
			ready <- fileTransferReady{name: name, size: size}
			announced = true
			_, err = io.Copy(writer, source)
			return nil, err
		})
		if !announced {
			ready <- fileTransferReady{err: err}
		}
		s.err = err
		if upload {
			_ = reader.CloseWithError(err)
		} else {
			_ = writer.CloseWithError(err)
		}
	}()
	select {
	case r := <-ready:
		return r, r.err
	case <-s.ctx.Done():
		return fileTransferReady{}, s.ctx.Err()
	}
}

func (b *Backend) finishTransfer(ctx context.Context, s *transferSession) (result any, retErr error) {
	if s.writer == nil && s.spool == nil {
		return nil, api.Fail("invalid_argument", "transfer is not an upload")
	}
	if s.finished {
		return nil, api.Fail("invalid_argument", "upload has already finished")
	}
	if s.header != nil && s.bytes != s.header.Size {
		return nil, api.Fail("invalid_argument", "upload does not match the declared file size")
	}
	if s.header == nil && s.bytes == 0 {
		return nil, api.Fail("invalid_argument", "archive must not be empty")
	}
	s.finished = true
	defer func() { retErr = errors.Join(retErr, s.abort()) }()
	if s.spool != nil {
		if _, err := s.spool.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek volume restore spool: %w", err)
		}
		if err := validateVolumeArchiveStream(s.spool); err != nil {
			return nil, err
		}
		if _, err := s.spool.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek validated volume restore: %w", err)
		}
		if b.id == "windows" {
			if err := filetool.ValidateArchive(s.spool); err != nil {
				return nil, fmt.Errorf("validate Windows volume backup: %w", err)
			}
			if _, err := s.spool.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("seek validated Windows volume restore: %w", err)
			}
		}
		_, err := b.withVolumeContainer(s.ctx, s.volume, func(id, root string) (any, error) {
			return nil, b.restoreVolumeStream(s.ctx, id, root, s.spool, s.bytes, true)
		})
		return nil, err
	}
	if s.tarWriter != nil {
		if !s.started {
			if err := s.tarWriter.WriteHeader(s.header); err != nil {
				return nil, fmt.Errorf("start file upload: %w", err)
			}
		}
		if err := s.tarWriter.Close(); err != nil {
			return nil, fmt.Errorf("finish file archive: %w", err)
		}
	}
	closeErr := s.writer.Close()
	select {
	case <-s.done:
		return s.result, errors.Join(closeErr, s.err)
	case <-ctx.Done():
		return nil, errors.Join(closeErr, ctx.Err())
	}
}

// streamVolumeBackup keeps the utility container alive until the normalized tar
// has been consumed and its removal has completed.
func (b *Backend) streamVolumeBackup(ctx context.Context, name string, dst io.Writer) error {
	_, err := b.withVolumeContainer(ctx, name, func(id, root string) (any, error) {
		if b.id == "windows" {
			archive, _, err := b.openFileToolRead(ctx, id, []string{"archive", root})
			if err != nil {
				return nil, fmt.Errorf("read volume archive: %w", err)
			}
			defer archive.Close()
			return nil, normalizeVolumeArchiveStream(archive, dst, "ucd")
		}
		archive, err := b.docker.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: root})
		if err != nil {
			return nil, fmt.Errorf("read volume archive: %w", err)
		}
		defer archive.Content.Close()
		return nil, normalizeVolumeArchiveStream(archive.Content, dst, "ucd")
	})
	return err
}

func normalizeVolumeArchiveStream(src io.Reader, dst io.Writer, root string) error {
	tracked := &volumeArchiveReader{Reader: src}
	reader, writer := tar.NewReader(tracked), tar.NewWriter(dst)
	count := 0
	for {
		before := tracked.bytes
		header, err := reader.Next()
		if err == io.EOF {
			if err := finishVolumeArchive(tracked, before); err != nil {
				return err
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read volume archive: %w", err)
		}
		count++
		if count > volumeEntryLimit {
			return fmt.Errorf("volume archive contains more than %d entries", volumeEntryLimit)
		}
		name := strings.TrimPrefix(header.Name, "./")
		if name == root || name == root+"/" {
			continue
		}
		if !strings.HasPrefix(name, root+"/") {
			return fmt.Errorf("volume archive has unexpected root %q", header.Name)
		}
		header.Name = strings.TrimPrefix(name, root+"/")
		if header.Typeflag == tar.TypeLink {
			link := strings.TrimPrefix(header.Linkname, "./")
			if !strings.HasPrefix(link, root+"/") {
				return fmt.Errorf("volume archive contains a hard link outside its root")
			}
			header.Linkname = strings.TrimPrefix(link, root+"/")
		}
		if err := validateVolumeHeader(header); err != nil {
			return err
		}
		// PAX path fields must follow the normalized header rather than the
		// original utility-container path.
		delete(header.PAXRecords, "path")
		delete(header.PAXRecords, "linkpath")
		if err := writer.WriteHeader(header); err != nil {
			return fmt.Errorf("write volume backup header: %w", err)
		}
		if _, err := io.Copy(writer, reader); err != nil {
			return fmt.Errorf("write volume backup content: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish volume backup: %w", err)
	}
	return nil
}

func validateVolumeHeader(header *tar.Header) error {
	name := strings.TrimSuffix(header.Name, "/")
	if name == "" || len(name) > 4096 || name != path.Clean(name) || name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\x00\\:") {
		return fmt.Errorf("volume backup contains an unsafe path")
	}
	if header.Size < 0 {
		return fmt.Errorf("volume backup contains an invalid file size")
	}
	for key := range header.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse") || key == "SCHILY.realsize" {
			return fmt.Errorf("volume backup contains an unsupported sparse file")
		}
	}
	switch header.Typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
	case tar.TypeSymlink:
		// A symlink is stored as data. Validation below prevents any archive
		// entry from traversing it during extraction.
		if header.Linkname == "" || strings.ContainsRune(header.Linkname, 0) {
			return fmt.Errorf("volume backup contains an invalid symlink")
		}
	case tar.TypeLink:
		link := header.Linkname
		if link == "" || strings.HasPrefix(link, "/") || strings.ContainsAny(link, "\x00\\:") {
			return fmt.Errorf("volume backup contains an unsafe link")
		}
		link = path.Clean(link)
		if link == ".." || strings.HasPrefix(link, "../") || strings.HasPrefix(link, "/") {
			return fmt.Errorf("volume backup contains a link outside the volume")
		}
	default:
		return fmt.Errorf("volume backup contains an unsupported special file")
	}
	return nil
}

// validateVolumeArchiveStream consumes a complete tar without extracting it.
// Parent symlinks and hard-link targets are checked after every header is known.
func validateVolumeArchiveStream(src io.Reader) error {
	tracked := &volumeArchiveReader{Reader: src}
	reader := tar.NewReader(tracked)
	entries := make(map[string]byte)
	metadataBytes := 0
	for {
		before := tracked.bytes
		header, err := reader.Next()
		if err == io.EOF {
			if err := finishVolumeArchive(tracked, before); err != nil {
				return err
			}
			break
		}
		if err != nil {
			return fmt.Errorf("invalid volume backup: %w", err)
		}
		if len(entries) >= volumeEntryLimit {
			return fmt.Errorf("volume backup contains more than %d entries", volumeEntryLimit)
		}
		if err := validateVolumeHeader(header); err != nil {
			return err
		}
		name := strings.TrimSuffix(header.Name, "/")
		metadataBytes += len(name) + len(header.Linkname)
		if metadataBytes > volumeMetadataLimit {
			return fmt.Errorf("volume backup path metadata exceeds 64 MiB")
		}
		if _, exists := entries[name]; exists {
			return fmt.Errorf("volume backup contains duplicate paths")
		}
		if header.Typeflag == tar.TypeLink {
			target := path.Clean(header.Linkname)
			kind, exists := entries[target]
			if !exists || (kind != tar.TypeReg && kind != tar.TypeRegA && kind != tar.TypeLink) {
				return fmt.Errorf("volume backup hard link must target a preceding archived file")
			}
		}
		entries[name] = header.Typeflag
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return fmt.Errorf("read volume backup contents: %w", err)
		}
	}
	for name := range entries {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if kind, exists := entries[parent]; exists && kind != tar.TypeDir {
				return fmt.Errorf("volume backup contains an entry beneath a non-directory")
			}
		}
	}
	return nil
}

func finishVolumeArchive(tracked *volumeArchiveReader, before int64) error {
	if tracked.bytes-before < 1024 || tracked.tail != [1024]byte{} {
		return fmt.Errorf("volume backup is missing its complete tar end marker")
	}
	// Data after tar's end marker must not conceal unvalidated entries.
	var trailing [32 << 10]byte
	for {
		n, err := tracked.Read(trailing[:])
		for _, value := range trailing[:n] {
			if value != 0 {
				return fmt.Errorf("volume backup contains data after the archive")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read volume backup trailer: %w", err)
		}
	}
	return nil
}

// restoreVolumeStream applies a previously validated archive.
func (b *Backend) restoreVolumeStream(ctx context.Context, id, root string, src io.Reader, size int64, empty bool) error {
	if size < 0 {
		return api.Fail("invalid_argument", "volume archive size must be nonnegative")
	}
	if empty {
		if err := b.emptyVolume(ctx, id, root); err != nil {
			return fmt.Errorf("prepare volume restore (some contents may already have been deleted): %w", err)
		}
	}
	if b.id == "windows" {
		writer, done, err := b.openFileToolWrite(ctx, id, []string{"extract", root, strconv.FormatInt(size, 10)})
		if err != nil {
			return fmt.Errorf("start volume restore (volume may now be empty): %w", err)
		}
		_, copyErr := io.CopyN(writer, src, size)
		closeErr := writer.Close()
		if err := errors.Join(copyErr, closeErr, <-done); err != nil {
			return fmt.Errorf("restore volume contents (volume may now be empty or partially restored): %w", err)
		}
		return nil
	}
	// The HTTP transport may close request bodies; the caller owns the source.
	_, err := b.docker.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: root, Content: struct{ io.Reader }{src}, CopyUIDGID: b.id == "linux"})
	if err != nil {
		return fmt.Errorf("restore volume contents (volume may now be empty or partially restored): %w", err)
	}
	return nil
}

func (s *transferSession) abort() error {
	s.stopOnce.Do(func() {
		s.cancel()
		if s.reader != nil {
			_ = s.reader.Close()
		}
		if s.writer != nil {
			_ = s.writer.Close()
		}
		if s.spool != nil {
			name := s.spool.Name()
			if err := s.spool.Close(); err != nil {
				s.cleanupErr = fmt.Errorf("close temporary volume archive: %w", err)
			}
			if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
				s.cleanupErr = errors.Join(s.cleanupErr, fmt.Errorf("remove temporary volume archive: %w", err))
			}
		}
	})
	return s.cleanupErr
}

func (s *transferSession) wait() {
	if s.done != nil {
		<-s.done
	}
}

func (b *Backend) registerTransfer(s *transferSession) (string, error) {
	id := rand.Text()
	b.transferMu.Lock()
	defer b.transferMu.Unlock()
	if b.transfersClosed {
		return "", api.Fail("unavailable", "engine is shutting down")
	}
	if len(b.transfers) >= transferLimit {
		return "", api.Fail("unavailable", "too many open binary transfers")
	}
	if b.transfers == nil {
		b.transfers = make(map[string]*transferSession)
	}
	s.lastActive = time.Now()
	b.transfers[id] = s
	s.timer = time.AfterFunc(transferIdleTime, func() { b.expireTransfer(id, s) })
	return id, nil
}

func (b *Backend) expireTransfer(id string, s *transferSession) {
	b.transferMu.Lock()
	if b.transfers[id] != s {
		b.transferMu.Unlock()
		return
	}
	remaining := transferIdleTime - time.Since(s.lastActive)
	if s.active != 0 || remaining > 0 {
		if remaining <= 0 {
			remaining = transferIdleTime
		}
		s.timer.Reset(remaining)
		b.transferMu.Unlock()
		return
	}
	delete(b.transfers, id)
	b.transferMu.Unlock()
	if err := s.abort(); err != nil && b.events != nil {
		b.events.Publish(api.Event{Engine: b.id, Type: "transfer.error", Message: err.Error()})
	}
	s.wait()
}

func (b *Backend) useTransfer(id string) (*transferSession, error) {
	b.transferMu.Lock()
	defer b.transferMu.Unlock()
	s := b.transfers[id]
	if s == nil {
		return nil, api.Fail("not_found", "transfer is closed or expired")
	}
	s.active++
	s.lastActive = time.Now()
	return s, nil
}

func (b *Backend) releaseTransfer(s *transferSession) {
	b.transferMu.Lock()
	s.active--
	s.lastActive = time.Now()
	b.transferMu.Unlock()
}

func (b *Backend) removeTransfer(id string) error {
	b.transferMu.Lock()
	s := b.transfers[id]
	delete(b.transfers, id)
	if s != nil {
		s.timer.Stop()
	}
	b.transferMu.Unlock()
	if s != nil {
		err := s.abort()
		s.wait()
		return err
	}
	return nil
}

func (b *Backend) closeTransfers() error {
	b.transferMu.Lock()
	b.transfersClosed = true
	sessions := b.transfers
	b.transfers = nil
	for _, s := range sessions {
		s.timer.Stop()
	}
	b.transferMu.Unlock()
	var cleanupErr error
	for _, s := range sessions {
		cleanupErr = errors.Join(cleanupErr, s.abort())
	}
	for _, s := range sessions {
		s.wait()
	}
	return cleanupErr
}

func (b *Backend) transferCall(ctx context.Context, req api.Request) (any, error) {
	if req.Method == "transfer.open" {
		p, err := api.Decode[api.TransferOpenParams](req)
		if err != nil {
			return nil, err
		}
		return b.openTransfer(ctx, p)
	}
	if len(req.Params) > base64.StdEncoding.EncodedLen(transferChunkSize)+4096 {
		return nil, api.Fail("invalid_argument", "transfer request exceeds the chunk size limit")
	}
	p, err := api.Decode[api.TransferParams](req)
	if err != nil {
		return nil, err
	}
	if p.SessionID == "" {
		return nil, api.Fail("invalid_argument", "transfer session ID is required")
	}
	if req.Method == "transfer.close" {
		return nil, b.removeTransfer(p.SessionID)
	}
	s, err := b.useTransfer(p.SessionID)
	if err != nil {
		return nil, err
	}
	defer b.releaseTransfer(s)
	stop := context.AfterFunc(ctx, func() { _ = s.abort() })
	defer stop()
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch req.Method {
	case "transfer.read":
		if s.reader == nil {
			return nil, api.Fail("invalid_argument", "transfer is not a download")
		}
		if p.MaxBytes == 0 {
			p.MaxBytes = 64 << 10
		}
		if p.MaxBytes < 1 || p.MaxBytes > transferChunkSize {
			return nil, api.Fail("invalid_argument", "maxBytes must be between 1 and 262144")
		}
		if s.pendingErr != nil {
			return nil, fmt.Errorf("read transfer: %w", s.pendingErr)
		}
		if s.finished {
			return api.TransferReadResult{Data: []byte{}, EOF: true}, nil
		}
		data := make([]byte, p.MaxBytes)
		var n int
		var readErr error
		for attempts := 0; attempts < 100; attempts++ {
			n, readErr = s.reader.Read(data)
			if n != 0 || readErr != nil || ctx.Err() != nil {
				break
			}
		}
		if n == 0 && readErr == nil && ctx.Err() == nil {
			readErr = io.ErrNoProgress
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readErr != nil {
			s.finished = true
			if readErr != io.EOF {
				s.pendingErr = readErr
			}
			s.abort()
		}
		if n == 0 && s.pendingErr != nil {
			return nil, fmt.Errorf("read transfer: %w", s.pendingErr)
		}
		return api.TransferReadResult{Data: data[:n], EOF: s.finished && s.pendingErr == nil}, nil
	case "transfer.write":
		if s.writer == nil && s.spool == nil {
			return nil, api.Fail("invalid_argument", "transfer is not an upload")
		}
		if s.finished {
			return nil, api.Fail("invalid_argument", "upload has already finished")
		}
		if len(p.Data) > transferChunkSize {
			return nil, api.Fail("invalid_argument", "chunk exceeds 262144 bytes")
		}
		if s.header != nil && int64(len(p.Data)) > s.header.Size-s.bytes {
			return nil, api.Fail("invalid_argument", "upload exceeds the declared file size")
		}
		if len(p.Data) == 0 {
			return nil, nil
		}
		var writer io.Writer = s.writer
		if s.spool != nil {
			writer = s.spool
		} else if s.tarWriter != nil {
			if !s.started {
				if err := s.tarWriter.WriteHeader(s.header); err != nil {
					return nil, fmt.Errorf("start file upload: %w", err)
				}
				s.started = true
			}
			writer = s.tarWriter
		}
		n, writeErr := writer.Write(p.Data)
		s.bytes += int64(n)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if writeErr != nil {
			return nil, fmt.Errorf("write transfer: %w", writeErr)
		}
		if n != len(p.Data) {
			return nil, fmt.Errorf("write transfer: %w", io.ErrShortWrite)
		}
		return nil, nil
	case "transfer.finish":
		return b.finishTransfer(ctx, s)
	default:
		return nil, api.Fail("invalid_argument", "unknown transfer method")
	}
}
