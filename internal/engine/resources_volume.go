package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/filetool"
)

func (b *Backend) volumeResourceCall(ctx context.Context, req api.Request) (any, error) {
	p, err := api.Decode[api.VolumeResourceParams](req)
	if err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, api.Fail("invalid_argument", "volume name is required")
	}
	if req.Method == "volume.create" {
		result, err := b.docker.VolumeCreate(ctx, client.VolumeCreateOptions{Name: p.Name, Driver: p.Driver, DriverOpts: p.DriverOpts, Labels: p.Labels})
		if err != nil {
			return nil, fmt.Errorf("create volume: %w", err)
		}
		return normalizeVolume(result.Volume, b.id), nil
	}
	v, err := b.docker.VolumeInspect(ctx, p.Name, client.VolumeInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect volume: %w", err)
	}
	if req.Method == "volume.inspect" {
		containers, err := b.docker.ContainerList(ctx, client.ContainerListOptions{All: true})
		if err != nil {
			return nil, fmt.Errorf("inspect volume attachments: %w", err)
		}
		details := api.VolumeDetails{Volume: normalizeVolume(v.Volume, b.id), Containers: []api.Container{}}
		for _, c := range containers.Items {
			for _, m := range c.Mounts {
				if m.Type == mount.TypeVolume && m.Name == p.Name {
					name := ""
					if len(c.Names) > 0 {
						name = strings.TrimPrefix(c.Names[0], "/")
					}
					details.Containers = append(details.Containers, api.Container{ID: c.ID, Name: name, Engine: b.id, Image: c.Image, State: string(c.State), Status: c.Status})
					break
				}
			}
		}
		details.Volume.UsageCount = int64(len(details.Containers))
		return details, nil
	}
	if req.Method == "volume.clone" {
		if p.Target == "" || p.Target == p.Name {
			return nil, api.Fail("invalid_argument", "a different target volume name is required")
		}
		return b.cloneVolume(ctx, p.Name, p.Target)
	}
	if req.Method == "volume.backup" {
		data, err := b.volumeBackup(ctx, p.Name)
		if err != nil {
			return nil, err
		}
		return api.ResourceData{Data: data, Name: p.Name + ".tar"}, nil
	}
	if (req.Method == "volume.empty" || req.Method == "volume.restore") && !p.Confirmed {
		return nil, api.Fail("confirmation_required", "confirm before replacing or deleting volume contents")
	}
	if req.Method == "volume.restore" {
		if len(p.Data) == 0 || len(p.Data) > maxResourceTransfer {
			return nil, api.Fail("invalid_argument", "backup must contain 1 byte to 8 MiB")
		}
		if _, err := validateVolumeArchive(p.Data); err != nil {
			return nil, err
		}
		if b.id == "windows" {
			if err := filetool.ValidateArchive(bytes.NewReader(p.Data)); err != nil {
				return nil, fmt.Errorf("validate Windows volume backup: %w", err)
			}
		}
	}
	result, err := b.withVolumeContainer(ctx, p.Name, func(id, root string) (any, error) {
		switch req.Method {
		case "volume.empty":
			if err := b.emptyVolume(ctx, id, root); err != nil {
				return nil, fmt.Errorf("empty volume (some contents may already have been deleted): %w", err)
			}
			return nil, nil
		case "volume.restore":
			return nil, b.restoreVolume(ctx, id, root, p.Data, true)
		default:
			if !strings.HasPrefix(req.Method, "volume.files.") {
				return nil, api.Fail("invalid_argument", "unknown volume method: "+req.Method)
			}
			method := strings.TrimPrefix(req.Method, "volume.files.")
			switch method {
			case "list", "read", "write", "upload", "remove", "mkdir", "download", "archive":
			default:
				return nil, api.Fail("invalid_argument", "unknown volume file method")
			}
			mapped, err := volumePath(root, p.Path)
			if err != nil {
				return nil, err
			}
			if mapped == root && method != "list" && method != "archive" && method != "download" {
				return nil, api.Fail("invalid_argument", "operation requires a path inside the volume")
			}
			if len(p.Data) > maxResourceTransfer {
				return nil, api.Fail("invalid_argument", "file exceeds 8 MiB; use the Docker CLI for larger transfers")
			}
			request, err := api.NewRequest("container.files."+method, b.id, api.ContainerFileParams{ID: id, Path: mapped, Data: p.Data, Mode: p.Mode, Recursive: p.Recursive})
			if err != nil {
				return nil, err
			}
			result, err := b.containerFilesCall(ctx, request)
			if err != nil {
				return nil, err
			}
			if listing, ok := result.(api.ContainerFileList); ok {
				for i := range listing.Entries {
					entry := &listing.Entries[i]
					entry.Path = path.Join(strings.ReplaceAll(p.Path, "\\", "/"), entry.Name)
				}
				return listing, nil
			}
			return result, nil
		}
	})
	return result, err
}

func (b *Backend) cloneVolume(ctx context.Context, source, target string) (result any, retErr error) {
	if _, err := b.docker.VolumeInspect(ctx, target, client.VolumeInspectOptions{}); err == nil {
		return nil, api.Fail("invalid_argument", "target volume already exists")
	} else if !errdefs.IsNotFound(err) {
		return nil, fmt.Errorf("check target volume: %w", err)
	}
	archive, err := os.CreateTemp(b.dataRoot, ".ucd-volume-clone-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary volume archive: %w", err)
	}
	defer func() {
		closeErr := archive.Close()
		removeErr := os.Remove(archive.Name())
		if closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close temporary clone archive: %w", closeErr))
		}
		if removeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove temporary clone archive (target volume may have been created): %w", removeErr))
		}
	}()
	if err := b.streamVolumeBackup(ctx, source, archive); err != nil {
		return nil, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind volume archive: %w", err)
	}
	if err := validateVolumeArchiveStream(archive); err != nil {
		return nil, fmt.Errorf("validate volume archive: %w", err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind volume archive: %w", err)
	}
	if b.id == "windows" {
		if err := filetool.ValidateArchive(archive); err != nil {
			return nil, fmt.Errorf("validate Windows volume archive: %w", err)
		}
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewind Windows volume archive: %w", err)
		}
	}
	archiveInfo, err := archive.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect temporary volume archive: %w", err)
	}
	token, err := randomName()
	if err != nil {
		return nil, err
	}
	created, err := b.docker.VolumeCreate(ctx, client.VolumeCreateOptions{Name: target, Labels: map[string]string{"ucd.clone.token": token}})
	if err != nil {
		return nil, fmt.Errorf("create target volume: %w", err)
	}
	if created.Volume.Labels["ucd.clone.token"] != token {
		return nil, fmt.Errorf("target volume already exists; it was not modified")
	}
	_, err = b.withVolumeContainer(ctx, target, func(id, root string) (any, error) {
		// The HTTP transport closes request bodies; keep ownership of the spool
		// until the deferred close and removal below.
		return nil, b.restoreVolumeStream(ctx, id, root, struct{ io.Reader }{archive}, archiveInfo.Size(), false)
	})
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, cleanupErr := b.docker.VolumeRemove(cleanup, target, client.VolumeRemoveOptions{})
		if cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("remove incomplete target volume %s: %w", target, cleanupErr))
		}
		return nil, err
	}
	return normalizeVolume(created.Volume, b.id), nil
}

func volumePath(root, value string) (string, error) {
	if value == "" || value == "." {
		return root, nil
	}
	if value != path.Clean(value) || strings.HasPrefix(value, "/") || value == ".." || strings.HasPrefix(value, "../") || strings.ContainsAny(value, "\x00\\:") {
		return "", api.Fail("invalid_argument", "volume path must be a relative path inside the volume")
	}
	if strings.HasPrefix(root, "/") {
		return root + "/" + value, nil
	}
	return root + `\` + strings.ReplaceAll(value, "/", `\`), nil
}

var kernelVersion = regexp.MustCompile(`(?:^|[^0-9])10\.0(?:\.|\s+)(\d+)`)

func (b *Backend) withVolumeContainer(ctx context.Context, name string, fn func(id, root string) (any, error)) (result any, retErr error) {
	imageName, root, command := "busybox:1.37.0", "/ucd", []string{"sh", "-c", "while :; do sleep 3600; done"}
	if b.id == "windows" {
		info, err := b.docker.Info(ctx, client.InfoOptions{})
		if err != nil {
			return nil, fmt.Errorf("determine Windows container version: %w", err)
		}
		match := kernelVersion.FindStringSubmatch(info.Info.KernelVersion)
		if len(match) != 2 {
			return nil, fmt.Errorf("cannot select a Windows utility image for kernel version %q", info.Info.KernelVersion)
		}
		build, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("parse Windows kernel version: %w", err)
		}
		switch {
		case build >= 26100:
			imageName = "mcr.microsoft.com/windows/servercore:ltsc2025"
		case build >= 20348:
			imageName = "mcr.microsoft.com/windows/servercore:ltsc2022"
		case build >= 17763:
			imageName = "mcr.microsoft.com/windows/servercore:ltsc2019"
		default:
			return nil, fmt.Errorf("Windows kernel build %d is unsupported by the volume utility", build)
		}
		root, command = `C:\ucd`, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "while ($true) { Start-Sleep -Seconds 3600 }"}
	}
	if _, err := b.docker.ImageInspect(ctx, imageName); err != nil {
		if !errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("inspect volume utility image: %w", err)
		}
		b.events.Publish(api.Event{Engine: b.id, Type: "volume.progress", Message: "Pulling volume utility image " + imageName})
		stream, err := b.docker.ImagePull(ctx, imageName, client.ImagePullOptions{})
		if err != nil {
			return nil, fmt.Errorf("pull required volume utility image %s: %w", imageName, err)
		}
		if err := stream.Wait(ctx); err != nil {
			return nil, fmt.Errorf("pull required volume utility image %s: %w", imageName, err)
		}
	}
	created, err := b.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: &container.Config{Image: imageName, Cmd: command}, HostConfig: &container.HostConfig{NetworkMode: "none", Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: root, VolumeOptions: &mount.VolumeOptions{NoCopy: true}}}}})
	if err != nil {
		return nil, fmt.Errorf("create volume utility container: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, err := b.docker.ContainerRemove(cleanup, created.ID, client.ContainerRemoveOptions{Force: true})
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove volume utility container %s: %w", created.ID, err))
		}
	}()
	if b.id == "windows" {
		if _, err := b.prepareFileTool(ctx, created.ID); err != nil {
			return nil, fmt.Errorf("prepare Windows volume utility before start: %w", err)
		}
	}
	if _, err := b.docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("start volume utility container: %w", err)
	}
	return fn(created.ID, root)
}

func (b *Backend) volumeBackup(ctx context.Context, name string) ([]byte, error) {
	if b.id == "windows" {
		var backup limitedVolumeBackup
		if err := b.streamVolumeBackup(ctx, name, &backup); err != nil {
			return nil, err
		}
		if _, err := validateVolumeArchive(backup.Bytes()); err != nil {
			return nil, err
		}
		return backup.Bytes(), nil
	}
	result, err := b.withVolumeContainer(ctx, name, func(id, root string) (any, error) {
		archive, err := b.docker.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: root})
		if err != nil {
			return nil, fmt.Errorf("read volume archive: %w", err)
		}
		defer archive.Content.Close()
		data, err := readResourceData(archive.Content)
		if err != nil {
			return nil, fmt.Errorf("read volume archive: %w", err)
		}
		return normalizeVolumeArchive(data, "ucd")
	})
	if err != nil {
		return nil, err
	}
	return result.([]byte), nil
}

type limitedVolumeBackup struct{ buffer bytes.Buffer }

func (b *limitedVolumeBackup) Bytes() []byte { return b.buffer.Bytes() }
func (b *limitedVolumeBackup) Len() int      { return b.buffer.Len() }

func (b *limitedVolumeBackup) Write(data []byte) (int, error) {
	if len(data) > maxResourceTransfer-b.Len() {
		return 0, fmt.Errorf("volume backup exceeds 8 MiB; use streaming transfer for larger backups")
	}
	return b.buffer.Write(data)
}

func (b *Backend) emptyVolume(ctx context.Context, id, root string) error {
	request, _ := api.NewRequest("container.files.list", b.id, api.ContainerFileParams{ID: id, Path: root})
	result, err := b.containerFilesCall(ctx, request)
	if err != nil {
		return fmt.Errorf("list volume contents: %w", err)
	}
	listing, ok := result.(api.ContainerFileList)
	if !ok {
		return fmt.Errorf("unexpected volume listing result")
	}
	for _, entry := range listing.Entries {
		if entry.Name == "" || entry.Name == "." || entry.Name == ".." || strings.ContainsAny(entry.Name, `/\:`) {
			return fmt.Errorf("volume entry has an unsafe name")
		}
		child, err := volumePath(root, entry.Name)
		if err != nil {
			return err
		}
		request, _ := api.NewRequest("container.files.remove", b.id, api.ContainerFileParams{ID: id, Path: child, Recursive: true})
		if _, err := b.containerFilesCall(ctx, request); err != nil {
			return fmt.Errorf("empty volume at %q: %w", entry.Name, err)
		}
	}
	request, _ = api.NewRequest("container.files.list", b.id, api.ContainerFileParams{ID: id, Path: root})
	result, err = b.containerFilesCall(ctx, request)
	if err != nil {
		return fmt.Errorf("verify volume is empty: %w", err)
	}
	listing, ok = result.(api.ContainerFileList)
	if !ok || len(listing.Entries) != 0 {
		return fmt.Errorf("volume was modified while emptying; inspect its contents and retry")
	}
	return nil
}

func (b *Backend) restoreVolume(ctx context.Context, id, root string, data []byte, empty bool) error {
	if _, err := validateVolumeArchive(data); err != nil {
		return err
	}
	if b.id == "windows" {
		if err := filetool.ValidateArchive(bytes.NewReader(data)); err != nil {
			return fmt.Errorf("validate Windows volume backup: %w", err)
		}
		return b.restoreVolumeStream(ctx, id, root, bytes.NewReader(data), int64(len(data)), empty)
	}
	if empty {
		if err := b.emptyVolume(ctx, id, root); err != nil {
			return fmt.Errorf("prepare volume restore (some contents may already have been deleted): %w", err)
		}
	}
	_, err := b.docker.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: root, Content: bytes.NewReader(data), CopyUIDGID: b.id == "linux"})
	if err != nil {
		return fmt.Errorf("restore volume contents (volume may now be empty or partially restored): %w", err)
	}
	return nil
}

// Backups use relative archive names so the same format can be restored to any volume.
func normalizeVolumeArchive(data []byte, root string) ([]byte, error) {
	reader := tar.NewReader(bytes.NewReader(data))
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	var total int64
	count := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read volume archive: %w", err)
		}
		count++
		total += header.Size
		if count > 10000 || header.Size < 0 || total > maxResourceTransfer {
			return nil, fmt.Errorf("volume archive exceeds the 8 MiB or 10000-entry limit")
		}
		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir, tar.TypeSymlink, tar.TypeLink:
		default:
			return nil, fmt.Errorf("volume archive contains an unsupported special file")
		}
		name := strings.TrimPrefix(strings.TrimPrefix(header.Name, "./"), root+"/")
		if name == root || name == "." || name == "" {
			continue
		}
		if name == header.Name && !strings.HasPrefix(header.Name, root+"/") && !strings.HasPrefix(header.Name, "./"+root+"/") {
			return nil, fmt.Errorf("volume archive has unexpected root %q", header.Name)
		}
		header.Name = name
		if header.Typeflag == tar.TypeLink {
			header.Linkname = strings.TrimPrefix(strings.TrimPrefix(header.Linkname, "./"), root+"/")
		}
		if err := writer.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write volume backup: %w", err)
		}
		if _, err := io.Copy(writer, reader); err != nil {
			return nil, fmt.Errorf("write volume backup: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("write volume backup: %w", err)
	}
	if _, err := validateVolumeArchive(buf.Bytes()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func validateVolumeArchive(data []byte) (int, error) {
	if len(data) > maxResourceTransfer {
		return 0, fmt.Errorf("volume backup exceeds 8 MiB")
	}
	if err := validateVolumeArchiveStream(bytes.NewReader(data)); err != nil {
		return 0, err
	}
	reader := tar.NewReader(bytes.NewReader(data))
	count := 0
	for {
		_, err := reader.Next()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return 0, fmt.Errorf("invalid volume backup: %w", err)
		}
		count++
		if count > 10000 {
			return 0, fmt.Errorf("volume backup contains more than 10000 entries")
		}
	}
}
