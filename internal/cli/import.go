package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/skyri/ucd/internal/api"
)

const (
	maxContextBytes = 56 << 20
	maxContextFiles = 10000
	maxContextPaths = 4 << 20
	maxRequestBytes = 80 << 20
)

func parseImport(args []string) (invocation, error) {
	f := flags("project import")
	engine := f.String("engine", "", "")
	name := f.String("name", "", "")
	id := f.String("id", "", "")
	replace := f.Bool("replace", false, "")
	file := f.String("file", "", "")
	f.StringVar(file, "f", "", "")
	if err := f.Parse(args); err != nil {
		return invocation{}, err
	}
	if f.NArg() > 1 {
		return invocation{}, errors.New("project import accepts at most one directory")
	}
	if *engine == "" {
		return invocation{}, errors.New("--engine is required")
	}
	if *replace && *id == "" {
		return invocation{}, errors.New("--replace requires --id")
	}
	dir := ""
	if f.NArg() == 1 {
		dir = f.Arg(0)
	}
	p, err := projectImport(dir, *file, *name, *id, *engine)
	if err != nil {
		return invocation{}, err
	}
	p.Replace = *replace
	return invocation{method: "project.import", engine: *engine, params: p}, nil
}

// importLocation resolves the explicitly selected directory and Compose file.
// Compose files must be in the context root so their relative paths retain
// their meaning after the service imports them.
func importLocation(dir, file string) (string, string, error) {
	var err error
	if dir == "" && file != "" {
		file, err = filepath.Abs(file)
		if err != nil {
			return "", "", fmt.Errorf("resolve compose file: %w", err)
		}
		dir = filepath.Dir(file)
	} else {
		if dir == "" {
			dir = "."
		}
		dir, err = filepath.Abs(dir)
		if err != nil {
			return "", "", fmt.Errorf("resolve project directory: %w", err)
		}
		if file != "" && !filepath.IsAbs(file) {
			file = filepath.Join(dir, file)
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", "", fmt.Errorf("inspect project directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", errors.New("project directory must be a real directory, not a symbolic link")
	}
	if file == "" {
		for _, candidate := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
			candidatePath := filepath.Join(dir, candidate)
			_, err := os.Lstat(candidatePath)
			if err == nil {
				file = candidatePath
				break
			}
			if !os.IsNotExist(err) {
				return "", "", fmt.Errorf("inspect %s: %w", candidatePath, err)
			}
		}
		if file == "" {
			return "", "", errors.New("no compose.yaml, compose.yml, docker-compose.yaml, or docker-compose.yml found; use --file")
		}
	}
	file, err = filepath.Abs(file)
	if err != nil {
		return "", "", fmt.Errorf("resolve compose file: %w", err)
	}
	relative, err := filepath.Rel(dir, file)
	if err != nil || relative == "." || filepath.Base(relative) != relative {
		return "", "", errors.New("the Compose file must be directly inside the project directory")
	}
	return dir, relative, nil
}

func defaultProjectName(dir string) string {
	base := strings.ToLower(filepath.Base(dir))
	var name strings.Builder
	for _, c := range base {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			name.WriteRune(c)
		} else {
			name.WriteByte('-')
		}
	}
	value := strings.Trim(name.String(), "-")
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		value = "project-" + value
	}
	if len(value) > 63 {
		value = strings.TrimRight(value[:63], "-")
	}
	return value
}

func projectImport(dir, file, name, id, engine string) (api.ProjectImport, error) {
	var p api.ProjectImport
	if !api.ValidEngine(engine) {
		return p, errors.New("engine must be linux or windows")
	}
	dir, composeFile, err := importLocation(dir, file)
	if err != nil {
		return p, err
	}
	if name == "" {
		name = defaultProjectName(dir)
	}
	if strings.TrimSpace(name) == "" || len(name) > 128 {
		return p, errors.New("project name must contain 1 to 128 characters")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return p, fmt.Errorf("open project directory: %w", err)
	}
	defer root.Close()
	var files []api.File
	var yaml []byte
	composeFound := false
	totalBytes, totalPaths := int64(0), 0
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk project directory: %w", walkErr)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("project context contains symbolic link %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("project context contains a non-regular file: %s", path)
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("resolve project file: %w", err)
		}
		totalPaths += len(relative)
		if totalPaths > maxContextPaths {
			return errors.New("project context file paths exceed 4 MiB")
		}
		portable := filepath.ToSlash(relative)
		if !fs.ValidPath(portable) || strings.ContainsAny(portable, `\:`) {
			return fmt.Errorf("project file has an invalid portable path: %s", relative)
		}
		if strings.EqualFold(portable, ".ucd-import.json") {
			return errors.New("project context contains .ucd-import.json, a filename reserved by the service; rename or remove it before importing")
		}
		if engine == "linux" && len(portable) > 1024 {
			return fmt.Errorf("Linux project file path exceeds 1,024 bytes: %s", relative)
		}
		original, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect project file %s: %w", relative, err)
		}
		if original.Size() > maxContextBytes-totalBytes {
			return errors.New("project context exceeds 56 MiB; select a smaller directory")
		}
		isComposeFile := relative == composeFile || runtime.GOOS == "windows" && strings.EqualFold(relative, composeFile)
		if engine == "linux" && isComposeFile && original.Size() > 4<<20 {
			return errors.New("Linux Compose file exceeds 4 MiB")
		}
		if engine == "linux" && !strings.EqualFold(relative, "compose.yaml") && original.Size() > 32<<20 {
			return fmt.Errorf("Linux project file %s exceeds 32 MiB", relative)
		}
		f, err := root.Open(relative)
		if err != nil {
			return fmt.Errorf("open project file %s: %w", relative, err)
		}
		opened, err := f.Stat()
		if err != nil {
			f.Close()
			return fmt.Errorf("inspect open project file %s: %w", relative, err)
		}
		if !opened.Mode().IsRegular() || !os.SameFile(original, opened) {
			f.Close()
			return fmt.Errorf("project file changed during import: %s", relative)
		}
		data, readErr := io.ReadAll(io.LimitReader(f, maxContextBytes-totalBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			return fmt.Errorf("read project file %s: %w", relative, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close project file %s: %w", relative, closeErr)
		}
		totalBytes += int64(len(data))
		if totalBytes > maxContextBytes {
			return errors.New("project context exceeds 56 MiB; select a smaller directory")
		}
		if isComposeFile {
			yaml = data
			composeFound = true
		}
		// The service stores the selected file as compose.yaml. Supplying the
		// same path in Files would conflict with that canonical copy.
		if strings.EqualFold(relative, "compose.yaml") {
			if !isComposeFile {
				return errors.New("the project already contains compose.yaml; select it as the Compose file or use a separate project directory")
			}
			return nil
		}
		limit := maxContextFiles
		if engine == "linux" {
			limit = 512
		}
		if len(files) >= limit {
			return fmt.Errorf("project context exceeds %d files", limit)
		}
		if engine == "linux" && len(data) > 32<<20 {
			return fmt.Errorf("Linux project file %s exceeds 32 MiB", relative)
		}
		files = append(files, api.File{Path: portable, Content: data, Mode: uint32(opened.Mode().Perm())})
		return nil
	})
	if err != nil {
		return p, err
	}
	if !composeFound {
		return p, fmt.Errorf("Compose file %s was not found in the project directory", composeFile)
	}
	if len(yaml) > 8<<20 {
		return p, errors.New("Compose file exceeds 8 MiB")
	}
	if engine == "linux" && len(yaml) > 4<<20 {
		return p, errors.New("Linux Compose file exceeds 4 MiB")
	}
	if !utf8.Valid(yaml) {
		return p, errors.New("Compose file must be UTF-8")
	}
	p = api.ProjectImport{ID: id, Name: name, Engine: engine, YAML: string(yaml), Files: files}
	request, err := api.NewRequest("project.import", engine, p)
	if err != nil {
		return api.ProjectImport{}, fmt.Errorf("encode project context: %w", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return api.ProjectImport{}, fmt.Errorf("encode project request: %w", err)
	}
	if len(encoded) > maxRequestBytes {
		return api.ProjectImport{}, errors.New("encoded project context exceeds the service's 80 MiB limit")
	}
	return p, nil
}

func compose(ctx context.Context, caller api.Caller, args []string) (any, string, error) {
	f := flags("compose")
	engine := f.String("engine", "", "")
	file := f.String("file", "", "")
	f.StringVar(file, "f", "", "")
	name := f.String("project-name", "", "")
	f.StringVar(name, "p", "", "")
	if err := f.Parse(args); err != nil {
		return nil, "", err
	}
	if !api.ValidEngine(*engine) {
		return nil, "", errors.New("compose requires --engine linux or windows")
	}
	if f.NArg() == 0 {
		return nil, "", errors.New("compose action is required")
	}
	action := f.Arg(0)
	switch action {
	case "up", "start", "stop", "restart", "down", "build", "pull", "logs":
	default:
		return nil, "", fmt.Errorf("unknown compose action %q", action)
	}
	actionFlags := flags("compose " + action)
	detached := actionFlags.Bool("d", false, "")
	volumes := actionFlags.Bool("volumes", false, "")
	if err := actionFlags.Parse(f.Args()[1:]); err != nil {
		return nil, "", err
	}
	if actionFlags.NArg() != 0 {
		return nil, "", errors.New("unexpected compose arguments")
	}
	if *detached && action != "up" {
		return nil, "", errors.New("-d is only valid for compose up")
	}
	if *volumes && action != "down" {
		return nil, "", errors.New("--volumes is only valid for compose down")
	}
	dir, _, err := importLocation("", *file)
	if err != nil {
		return nil, "", err
	}
	if *name == "" {
		*name = defaultProjectName(dir)
	}
	if strings.TrimSpace(*name) == "" || len(*name) > 128 {
		return nil, "", errors.New("project name must contain 1 to 128 characters")
	}
	projectsResult, err := call(ctx, caller, invocation{method: "project.list", params: struct{}{}})
	if err != nil {
		return nil, "", err
	}
	encoded, err := json.Marshal(projectsResult)
	if err != nil {
		return nil, "", fmt.Errorf("encode project list: %w", err)
	}
	var projects []api.Project
	if err := json.Unmarshal(encoded, &projects); err != nil {
		return nil, "", fmt.Errorf("decode project list: %w", err)
	}
	id := ""
	for _, project := range projects {
		if project.Discovered {
			continue
		}
		if project.Name != *name {
			continue
		}
		for _, part := range project.Parts {
			if part.Engine == *engine {
				if id != "" && id != project.ID {
					return nil, "", fmt.Errorf("multiple %s projects named %s; use project --id", *engine, *name)
				}
				id = project.ID
			}
		}
	}
	if action == "up" {
		p, err := projectImport("", *file, *name, id, *engine)
		if err != nil {
			return nil, "", err
		}
		p.Replace = id != ""
		result, err := call(ctx, caller, invocation{method: "project.import", engine: *engine, params: p})
		if err != nil {
			return nil, "", err
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, "", fmt.Errorf("encode imported project: %w", err)
		}
		var project api.Project
		if err := json.Unmarshal(encoded, &project); err != nil {
			return nil, "", fmt.Errorf("decode imported project: %w", err)
		}
		if project.ID == "" {
			return nil, "", errors.New("service returned an imported project without an ID")
		}
		id = project.ID
	}
	if id == "" {
		return nil, "", fmt.Errorf("project %s has not been imported for %s; run compose up first", *name, *engine)
	}
	result, err := call(ctx, caller, invocation{method: "project.action", engine: *engine, params: api.ProjectParams{ID: id, Engine: *engine, Action: action, RemoveVolumes: *volumes}})
	return result, "project.action", err
}
