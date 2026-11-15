package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

const (
	maxYAMLBytes        = 4 << 20
	maxImportBytes      = 56 << 20
	maxProjectFileBytes = 32 << 20
	maxProjectFiles     = 512
	manifestName        = ".ucd-import.json"
)

var windowsBindPath = regexp.MustCompile(`(?i)^[a-z]:[/\\]|^\\\\`)

// loadProject applies the configured port-binding default to the Compose model.
func (b *Backend) loadProject(ctx context.Context, part api.ProjectPart) (*types.Project, error) {
	if err := b.validatePart(part, true); err != nil {
		return nil, err
	}
	root, err := b.projectsRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := noSymlinks(root, filepath.Join(part.Name, "compose.yaml")); err != nil {
		return nil, err
	}
	return b.loadStagedProject(ctx, part)
}

func (b *Backend) loadStagedProject(ctx context.Context, part api.ProjectPart) (*types.Project, error) {
	options := composeapi.ProjectLoadOptions{ProjectName: part.Name, WorkingDir: part.Directory, ConfigPaths: []string{part.ComposeFile}, Offline: true}
	if b.id == "linux" {
		options.ProjectOptionsFns = []cli.ProjectOptionsFn{cli.WithResolvedPaths(false)}
		unresolved, err := b.compose.LoadProject(ctx, options)
		if err != nil {
			return nil, fmt.Errorf("load Compose project: %w", err)
		}
		for name, svc := range unresolved.Services {
			for _, mount := range svc.Volumes {
				if mount.Type == types.VolumeTypeBind && windowsBindPath.MatchString(mount.Source) {
					return nil, fmt.Errorf("service %s uses Windows bind path %q on the Linux engine; use a path inside the Linux filesystem", name, mount.Source)
				}
			}
		}
		options.ProjectOptionsFns = nil
	}
	project, err := b.compose.LoadProject(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("load Compose project: %w", err)
	}
	defaultHostIP := "127.0.0.1"
	if b.RuntimeConfiguration().Network.DefaultPortBinding == api.DefaultPortBindingAll {
		defaultHostIP = "0.0.0.0"
	}
	for name, svc := range project.Services {
		for i := range svc.Ports {
			if svc.Ports[i].HostIP == "" {
				svc.Ports[i].HostIP = defaultHostIP
			}
		}
		project.Services[name] = svc
	}
	if b.id == "windows" {
		if _, err := windowsRuntimeProject(project); err != nil {
			return nil, err
		}
	}
	return project, nil
}

func projectPorts(project *types.Project) ([]api.Port, error) {
	ports := []api.Port{}
	for _, name := range project.ServiceNames() {
		for _, p := range project.Services[name].Ports {
			if p.Published == "" {
				continue
			}
			published, err := strconv.ParseUint(p.Published, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("service %s has published port %q; use an individual port rather than a range", name, p.Published)
			}
			if p.Target > 65535 {
				return nil, fmt.Errorf("service %s has invalid target port %d", name, p.Target)
			}
			ports = append(ports, api.Port{HostIP: p.HostIP, Published: uint16(published), Target: uint16(p.Target), Protocol: p.Protocol})
		}
	}
	return ports, nil
}

func (b *Backend) composeCall(ctx context.Context, req api.Request) (any, error) {
	if req.Method == "compose.logs.read" || req.Method == "compose.logs.close" {
		return b.outputSessionCall(ctx, req)
	}
	p, err := api.Decode[api.ComposeParams](req)
	if err != nil {
		return nil, err
	}
	switch req.Method {
	case "compose.validate", "compose.import":
		return b.importProject(ctx, p, req.Method == "compose.import")
	case "compose.read":
		if err := b.validatePart(p.Project, true); err != nil {
			return nil, err
		}
		root, err := b.projectsRoot()
		if err != nil {
			return nil, err
		}
		defer root.Close()
		name := filepath.Join(p.Project.Name, "compose.yaml")
		if err := noSymlinks(root, name); err != nil {
			return nil, err
		}
		data, err := root.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read Compose file: %w", err)
		}
		return api.ProjectFile{Engine: b.id, YAML: string(data)}, nil
	case "compose.write":
		return b.writeProject(ctx, p)
	case "compose.ports":
		project, err := b.loadProject(ctx, p.Project)
		if err != nil {
			return nil, err
		}
		project, err = selectComposeServices(project, p.Services, true)
		if err != nil {
			return nil, err
		}
		return projectPorts(project)
	case "compose.logs.open":
		project, err := b.loadProject(ctx, p.Project)
		if err != nil {
			return nil, err
		}
		project, err = selectComposeServices(project, p.Services, false)
		if err != nil {
			return nil, err
		}
		streamCtx, cancel, detach := newStreamContext(ctx)
		defer detach()
		id, err := b.addOutputSession(func() {}, cancel, func(w io.Writer) error {
			logs := &streamLogConsumer{out: w, cancel: cancel}
			err := b.compose.Logs(streamCtx, project.Name, logs, composeapi.LogOptions{Project: project, Services: p.Services, Tail: "500", Follow: true})
			logs.mu.Lock()
			writeErr := logs.err
			logs.mu.Unlock()
			if writeErr != nil {
				return writeErr
			}
			return err
		}, nil, "", nil, "compose.logs")
		if err != nil {
			return nil, err
		}
		if err := b.finishSessionOpen(ctx, id); err != nil {
			return nil, err
		}
		return api.ExecOpenResult{SessionID: id}, nil
	case "compose.action":
		project, err := b.loadProject(ctx, p.Project)
		if err != nil {
			return nil, err
		}
		if len(p.Services) != 0 && p.RemoveVolumes {
			return nil, api.Fail("invalid_argument", "removing volumes requires selecting the entire project")
		}
		project, err = selectComposeServices(project, p.Services, p.Action == "up")
		if err != nil {
			return nil, err
		}
		if _, err = projectPorts(project); err != nil {
			return nil, err
		}
		if b.id == "windows" {
			project, err = windowsRuntimeProject(project)
			if err != nil {
				return nil, err
			}
		}
		compose := b.compose
		var output *composeOutput
		if p.Action == "up" || p.Action == "build" || p.Action == "pull" {
			output = &composeOutput{engine: b.id, project: project.Name, events: b.events}
			defer output.flush()
			compose, err = b.newCompose(ctx, output, project.Name)
			if err != nil {
				return nil, fmt.Errorf("create Compose operation: %w", err)
			}
		}
		switch p.Action {
		case "up":
			err = compose.Up(ctx, project, composeapi.UpOptions{Create: composeapi.CreateOptions{Build: &composeapi.BuildOptions{Out: output, Progress: "plain"}, Services: p.Services, Inherit: true, Recreate: composeapi.RecreateDiverged, RecreateDependencies: composeapi.RecreateDiverged}})
		case "start":
			err = compose.Start(ctx, project.Name, composeapi.StartOptions{Project: project, Services: p.Services})
		case "down":
			err = compose.Down(ctx, project.Name, composeapi.DownOptions{Project: project, Volumes: p.RemoveVolumes, Services: p.Services})
		case "stop":
			err = compose.Stop(ctx, project.Name, composeapi.StopOptions{Project: project, Services: p.Services})
		case "restart":
			err = compose.Restart(ctx, project.Name, composeapi.RestartOptions{Project: project, Services: p.Services})
		case "pull":
			err = compose.Pull(ctx, project, composeapi.PullOptions{})
		case "build":
			err = compose.Build(ctx, project, composeapi.BuildOptions{Out: output, Progress: "plain", Services: p.Services})
		case "logs":
			logs := &logConsumer{}
			err = b.compose.Logs(ctx, project.Name, logs, composeapi.LogOptions{Project: project, Tail: "500"})
			if err != nil {
				return nil, fmt.Errorf("Compose logs: %w", err)
			}
			return api.LogResult{Text: logs.String()}, nil
		default:
			return nil, api.Fail("invalid_argument", "unknown Compose action: "+p.Action)
		}
		if err != nil {
			err = fmt.Errorf("Compose %s: %w", p.Action, err)
		}
		if err == nil && b.id == "linux" && (p.Action == "up" || p.Action == "start" || p.Action == "restart") {
			if portErr := b.waitLinuxProjectPorts(ctx, project.Name); portErr != nil {
				err = portErr
			}
		}
		if b.id == "windows" && (p.Action == "up" || p.Action == "start" || p.Action == "down" || p.Action == "stop" || p.Action == "restart") {
			if portErr := b.reconcileActionPorts(ctx, func(c container.Summary) bool {
				return c.Labels[composeapi.ProjectLabel] == project.Name
			}); portErr != nil {
				return nil, errors.Join(err, fmt.Errorf("reconcile published ports: %w", portErr))
			}
		}
		if err != nil {
			return nil, err
		}
		return nil, nil
	default:
		return nil, api.Fail("invalid_argument", "unknown Compose method: "+req.Method)
	}
}

func selectComposeServices(project *types.Project, services []string, dependencies bool) (*types.Project, error) {
	if len(services) == 0 {
		return project, nil
	}
	project, err := project.WithServicesEnabled(services...)
	if err != nil {
		return nil, fmt.Errorf("select Compose services: %w", err)
	}
	if dependencies {
		project, err = project.WithSelectedServices(services, types.IncludeDependencies)
	} else {
		project, err = project.WithSelectedServices(services, types.IgnoreDependencies)
	}
	if err != nil {
		return nil, fmt.Errorf("select Compose services: %w", err)
	}
	return project, nil
}

// Compose build output is emitted as bounded, project-scoped events. It may
// contain application output, so never add configuration or registry data here.
type composeOutput struct {
	mu              sync.Mutex
	buf             []byte
	timer           *time.Timer
	engine, project string
	events          *api.EventBus
}

func (w *composeOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	w.publish(false, false)
	if len(w.buf) > 0 && w.timer == nil {
		w.timer = time.AfterFunc(150*time.Millisecond, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.timer = nil
			w.publish(true, false)
		})
	}
	return len(p), nil
}

func (w *composeOutput) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.publish(true, true)
}

func (w *composeOutput) publish(flush, final bool) {
	for len(w.buf) > 0 {
		if len(w.buf) < 4096 && !flush {
			return
		}
		n := min(len(w.buf), 4096)
		for n < len(w.buf) && n > 0 && !utf8.RuneStart(w.buf[n]) {
			n--
		}
		if !final && n == len(w.buf) {
			last := n - 1
			for last > 0 && !utf8.RuneStart(w.buf[last]) {
				last--
			}
			if !utf8.FullRune(w.buf[last:n]) {
				n = last
			}
		}
		if n == 0 {
			return
		}
		w.events.Publish(api.Event{Engine: w.engine, Project: w.project, Type: "compose.output", Message: string(w.buf[:n])})
		w.buf = w.buf[n:]
	}
}

type streamLogConsumer struct {
	mu     sync.Mutex
	out    io.Writer
	cancel context.CancelFunc
	err    error
}

func (c *streamLogConsumer) add(name, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	_, c.err = fmt.Fprintf(c.out, "%s | %s\n", name, message)
	if c.err != nil {
		c.cancel()
	}
}
func (c *streamLogConsumer) Log(name, message string)    { c.add(name, message) }
func (c *streamLogConsumer) Err(name, message string)    { c.add(name, message) }
func (c *streamLogConsumer) Status(name, message string) { c.add(name, message) }

type logConsumer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *logConsumer) add(name, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, fragment := range []string{name, " | ", message, "\n"} {
		remaining := (4 << 20) - c.buf.Len()
		if remaining <= 0 {
			break
		}
		if len(fragment) > remaining {
			fragment = fragment[:remaining]
		}
		c.buf.WriteString(fragment)
	}
}
func (c *logConsumer) Log(name, message string)    { c.add(name, message) }
func (c *logConsumer) Err(name, message string)    { c.add(name, message) }
func (c *logConsumer) Status(name, message string) { c.add(name, message) }
func (c *logConsumer) String() string              { c.mu.Lock(); defer c.mu.Unlock(); return c.buf.String() }

func (b *Backend) discover(ctx context.Context) ([]api.ProjectPart, error) {
	result, err := b.docker.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("discover Compose projects: %w", err)
	}
	byName := map[string]api.ProjectPart{}
	for _, c := range result.Items {
		name, dir, file := c.Labels[composeapi.ProjectLabel], c.Labels[composeapi.WorkingDirLabel], c.Labels[composeapi.ConfigFilesLabel]
		if name == "" {
			continue
		}
		// The contract displays one Compose file. Multiple-file or incomplete
		// discovered projects remain visible; managing them requires import.
		if first, _, ok := strings.Cut(file, ","); ok {
			file = first
		}
		byName[name] = api.ProjectPart{Engine: b.id, Name: name, Directory: dir, ComposeFile: file}
	}
	parts := make([]api.ProjectPart, 0, len(byName))
	for _, p := range byName {
		parts = append(parts, p)
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Name < parts[j].Name })
	return parts, nil
}

func randomName() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return ".ucd-" + hex.EncodeToString(b[:]), nil
}

// fileName accepts portable relative paths only, including on Windows.
func fileName(name string) (string, error) {
	if len(name) > 1024 || name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:\x00") || !filepath.IsLocal(name) {
		return "", fmt.Errorf("invalid project file path %q", name)
	}
	return filepath.Clean(name), nil
}

func noSymlinks(root *os.Root, name string) error {
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	path := ""
	for _, part := range parts {
		path = filepath.Join(path, part)
		info, err := root.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("project path %q contains a symbolic link", name)
		}
	}
	return nil
}

// Project directories must be traversable by non-root containers even when
// the helper's restrictive umask is in effect. Existing directories retain
// their permissions; only newly created directories are changed.
func createProjectDirs(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	name := ""
	for _, component := range strings.Split(filepath.Clean(dir), string(filepath.Separator)) {
		name = filepath.Join(name, component)
		info, err := root.Lstat(name)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("project directory %q is not a directory", name)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err := root.Mkdir(name, 0700); err != nil {
			return err
		}
		d, err := root.Open(name)
		if err != nil {
			return err
		}
		chmodErr := d.Chmod(0755)
		closeErr := d.Close()
		if chmodErr != nil {
			return chmodErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func writeFiles(root *os.Root, yaml string, files []api.File) error {
	if yaml == "" || len(yaml) > maxYAMLBytes {
		return api.Fail("invalid_argument", "Compose YAML is empty or exceeds 4 MiB")
	}
	if len(files) > maxProjectFiles {
		return fmt.Errorf("project transfer exceeds 512 files")
	}
	total := 0
	seen := map[string]bool{"compose.yaml": true, manifestName: true}
	for _, f := range files {
		name, err := fileName(f.Path)
		if err != nil {
			return err
		}
		if seen[strings.ToLower(name)] {
			return fmt.Errorf("duplicate project file %q", f.Path)
		}
		seen[strings.ToLower(name)] = true
		if len(f.Content) > maxProjectFileBytes {
			return fmt.Errorf("project file %q exceeds 32 MiB", f.Path)
		}
		total += len(f.Content)
		if total > maxImportBytes {
			return fmt.Errorf("project files exceed 56 MiB")
		}
		if err := noSymlinks(root, name); err != nil {
			return err
		}
		if err := createProjectDirs(root, filepath.Dir(name)); err != nil {
			return fmt.Errorf("create directory for %q: %w", f.Path, err)
		}
		out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("create project file %q: %w", f.Path, err)
		}
		_, writeErr := out.Write(f.Content)
		if writeErr == nil {
			writeErr = out.Chmod(0644 | fs.FileMode(f.Mode&0111))
		}
		closeErr := out.Close()
		if writeErr != nil {
			return fmt.Errorf("write project file %q: %w", f.Path, writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close project file %q: %w", f.Path, closeErr)
		}
	}
	return root.WriteFile("compose.yaml", []byte(yaml), 0600)
}

func (b *Backend) projectsRoot() (*os.Root, error) {
	path := filepath.Join(b.dataRoot, "projects")
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, fmt.Errorf("create project directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("project directory must not be a symbolic link")
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open project directory: %w", err)
	}
	return r, nil
}

func (b *Backend) importProject(ctx context.Context, p api.ComposeParams, save bool) (any, error) {
	if p.Project.Engine != b.id || !api.ValidName(p.Project.Name) {
		return nil, api.Fail("invalid_argument", "valid project name and engine are required")
	}
	if save {
		b.projectMu.Lock()
		defer b.projectMu.Unlock()
	}
	root, err := b.projectsRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if save {
		_, statErr := root.Lstat(p.Project.Name)
		if statErr == nil {
			if !p.Replace {
				return nil, fmt.Errorf("project %q already exists; use replace to update its files", p.Project.Name)
			}
			return b.replaceProject(ctx, root, p)
		}
		if !os.IsNotExist(statErr) {
			return nil, statErr
		}
	}
	tmp, err := randomName()
	if err != nil {
		return nil, err
	}
	if err = root.Mkdir(tmp, 0700); err != nil {
		return nil, fmt.Errorf("stage project: %w", err)
	}
	defer root.RemoveAll(tmp)
	projectDir, err := root.Open(tmp)
	if err != nil {
		return nil, err
	}
	chmodErr := projectDir.Chmod(0755)
	closeErr := projectDir.Close()
	if chmodErr != nil {
		return nil, fmt.Errorf("set project directory permissions: %w", chmodErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	stage, err := root.OpenRoot(tmp)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	if err = writeFiles(stage, p.YAML, p.Files); err != nil {
		return nil, err
	}
	part := api.ProjectPart{Engine: b.id, Name: p.Project.Name, Directory: filepath.Join(root.Name(), tmp), ComposeFile: filepath.Join(root.Name(), tmp, "compose.yaml")}
	project, err := b.loadStagedProject(ctx, part)
	if err != nil {
		return nil, err
	}
	ports, err := projectPorts(project)
	if err != nil {
		return nil, err
	}
	if !save {
		return ports, nil
	}
	if err := writeManifest(stage, p.Files); err != nil {
		return nil, err
	}
	if err = root.Rename(tmp, p.Project.Name); err != nil {
		return nil, fmt.Errorf("save project: %w", err)
	}
	part.Directory = filepath.Join(root.Name(), p.Project.Name)
	part.ComposeFile = filepath.Join(part.Directory, "compose.yaml")
	return part, nil
}

func (b *Backend) validatePart(part api.ProjectPart, managed bool) error {
	if part.Engine != b.id || part.Name == "" {
		return api.Fail("invalid_argument", "invalid project engine or name")
	}
	if !filepath.IsAbs(part.Directory) || !filepath.IsAbs(part.ComposeFile) {
		return api.Fail("invalid_argument", "discovered project location is incomplete; import this project to manage it")
	}
	if managed {
		if !api.ValidName(part.Name) || filepath.Clean(part.Directory) != filepath.Join(b.dataRoot, "projects", part.Name) || filepath.Clean(part.ComposeFile) != filepath.Join(part.Directory, "compose.yaml") {
			return api.Fail("invalid_argument", "only managed project files can be accessed; import this project first")
		}
	}
	return nil
}

func (b *Backend) writeProject(ctx context.Context, p api.ComposeParams) (any, error) {
	b.projectMu.Lock()
	defer b.projectMu.Unlock()
	if err := b.validatePart(p.Project, true); err != nil {
		return nil, err
	}
	if p.YAML == "" || len(p.YAML) > maxYAMLBytes {
		return nil, api.Fail("invalid_argument", "Compose YAML is empty or exceeds 4 MiB")
	}
	root, err := b.projectsRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err = noSymlinks(root, p.Project.Name); err != nil {
		return nil, err
	}
	projectRoot, err := root.OpenRoot(p.Project.Name)
	if err != nil {
		return nil, fmt.Errorf("open project: %w", err)
	}
	defer projectRoot.Close()
	if err = noSymlinks(projectRoot, "compose.yaml"); err != nil {
		return nil, err
	}
	tmp, err := randomName()
	if err != nil {
		return nil, err
	}
	f, err := projectRoot.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer projectRoot.Remove(tmp)
	_, writeErr := io.WriteString(f, p.YAML)
	closeErr := f.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	staged := p.Project
	staged.ComposeFile = filepath.Join(p.Project.Directory, tmp)
	project, err := b.loadStagedProject(ctx, staged)
	if err != nil {
		return nil, err
	}
	if _, err = projectPorts(project); err != nil {
		return nil, err
	}
	if err = projectRoot.Rename(tmp, "compose.yaml"); err != nil {
		return nil, fmt.Errorf("replace Compose file: %w", err)
	}
	return api.ProjectFile{Engine: b.id, YAML: p.YAML}, nil
}
