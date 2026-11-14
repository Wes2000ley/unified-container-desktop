// Package cli implements the machine service's command-line client.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/skyri/ucd/internal/api"
)

const usage = `Unified Container Desktop (UCD)
Linux and Windows containers, side by side. One app, two shared machine-wide engines.
Manage containers with docker, or use ucd for app settings and infrastructure.

Commands:
  ucd web [--port N]
  ucd docker --engine linux|windows [DOCKER OPTIONS] COMMAND [ARG...]
  ucd plugins install
  ucd engine ls|status|start|stop [--engine linux|windows]
  ucd project ls|import|read|write|up|start|stop|restart|down|build|pull|logs|remove [options]
  ucd compose --engine linux|windows [-f compose.yaml] [-p name] up|start|stop|restart|down|build|pull|logs
  ucd container ls|inspect|logs|stats|start|stop|restart|rm [options]
  ucd image ls|pull|rm [options]
  ucd registry ls|login|logout [options]
  ucd volume ls|inspect|rm [options]
  ucd settings show|configure [--file settings.json]
  ucd wsl status|configure [--cpus N --memory-mb N --disk-gb N]
  ucd operator ls|add|remove [--account DOMAIN\user]
  ucd share ls|create|remove [--name NAME --path WINDOWS_PATH]
  ucd diagnostics
  ucd events [--after SEQUENCE] [--epoch EPOCH]

Use ucd docker for the upstream Docker CLI, including Compose and Buildx.
UCD keeps a separate Docker client config for this command and uses the shared
machine registry credentials. An explicit Docker --config selects that config
and credential store instead.
The installed docker command uses the same broker, defaults to Linux, and accepts
--engine windows to select the Windows engine.
Use ucd registry login --username NAME --password-stdin [--server HOST] to
store a registry sign-in without placing its password or token in the command line.
The ucd compose command manages imported UCD projects.
Use --engine for an engine-specific action. Project and container actions use
--id; image actions use --reference or --id; volume actions use --name.
Project import uses --engine, an optional --name, -f/--file and an optional
directory argument. Use --replace --id ID to refresh an existing engine part.
Import copies every regular file in that directory,
including dotfiles; select a directory without credentials or unwanted files.
Windows cannot reliably preserve POSIX executable bits; set them in the image.
Project write uses --id, --engine and -f/--file. Management results are JSON;
ucd docker uses Docker's native output and exit status.
For an event cursor, pass the previous batch's epoch with --after. If the epoch
is omitted or has changed, the service returns its retained events.
`

type invocation struct {
	method string
	engine string
	params any
}

// Run executes one CLI command and returns a process exit status.
func Run(ctx context.Context, caller api.Caller, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if args[0] == "docker" {
		return runDocker(ctx, caller, args[1:], os.Stdin, stdout, stderr)
	}
	if args[0] == "plugins" {
		return runPlugins(args[1:], stdout, stderr)
	}
	if args[0] == "registry" {
		return runRegistry(ctx, caller, args[1:], os.Stdin, stdout, stderr)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	result, method, err := execute(ctx, caller, args)
	if err != nil {
		fmt.Fprintln(stderr, "ucd:", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintln(stderr, "ucd: write result:", err)
		return 1
	}
	if method == "project.action" {
		data, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintln(stderr, "ucd: read operation result:", err)
			return 1
		}
		var operation struct {
			Success *bool `json:"success"`
		}
		if err := json.Unmarshal(data, &operation); err != nil || operation.Success == nil {
			fmt.Fprintln(stderr, "ucd: invalid project operation result")
			return 1
		}
		if !*operation.Success {
			fmt.Fprintln(stderr, "ucd: project action failed; see the per-engine results above")
			return 1
		}
	}
	if method == "container.list" || method == "image.list" || method == "volume.list" {
		data, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintln(stderr, "ucd: read inventory result:", err)
			return 1
		}
		// A cross-engine inventory is an object with an errors array; an
		// engine-specific inventory is a plain array.
		if len(data) > 0 && data[0] == '{' {
			var inventory struct {
				Errors []json.RawMessage `json:"errors"`
			}
			if err := json.Unmarshal(data, &inventory); err != nil {
				fmt.Fprintln(stderr, "ucd: invalid inventory result:", err)
				return 1
			}
			if len(inventory.Errors) > 0 {
				fmt.Fprintln(stderr, "ucd: inventory is incomplete; see the per-engine errors above")
				return 1
			}
		}
	}
	return 0
}

func execute(ctx context.Context, caller api.Caller, args []string) (any, string, error) {
	if args[0] == "compose" {
		return compose(ctx, caller, args[1:])
	}
	req, err := parse(args)
	if err != nil {
		return nil, "", err
	}
	result, err := call(ctx, caller, req)
	return result, req.method, err
}

func call(ctx context.Context, caller api.Caller, req invocation) (any, error) {
	if req.engine != "" && !api.ValidEngine(req.engine) {
		return nil, errors.New("engine must be linux or windows")
	}
	request, err := api.NewRequest(req.method, req.engine, req.params)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	result, err := caller.Call(ctx, request)
	if err == nil {
		return result, nil
	}
	var serviceError *api.Error
	if errors.As(err, &serviceError) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	return nil, fmt.Errorf("call the Unified Container Desktop service over its local named pipe: %w; verify the service is installed and running", err)
}

func flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f
}

func parse(args []string) (invocation, error) {
	if args[0] == "events" {
		f := flags("events")
		after := f.Uint64("after", 0, "")
		epoch := f.String("epoch", "", "")
		if err := f.Parse(args[1:]); err != nil {
			return invocation{}, err
		}
		if f.NArg() != 0 {
			return invocation{}, errors.New("unexpected arguments")
		}
		return invocation{method: "system.events", params: api.EventsParams{After: *after, Epoch: *epoch}}, nil
	}
	if args[0] == "diagnostics" {
		if len(args) != 1 {
			return invocation{}, errors.New("diagnostics does not accept arguments")
		}
		return invocation{method: "system.diagnostics", params: struct{}{}}, nil
	}
	if len(args) < 2 {
		return invocation{}, fmt.Errorf("missing action for %s; run ucd help", args[0])
	}
	switch args[0] {
	case "engine":
		return parseEngine(args[1], args[2:])
	case "project":
		return parseProject(args[1], args[2:])
	case "container":
		return parseContainer(args[1], args[2:])
	case "image":
		return parseImage(args[1], args[2:])
	case "volume":
		return parseVolume(args[1], args[2:])
	case "settings":
		return parseSettings(args[1], args[2:])
	case "wsl":
		return parseWSL(args[1], args[2:])
	case "operator":
		return parseOperator(args[1], args[2:])
	case "share":
		return parseShare(args[1], args[2:])
	default:
		return invocation{}, fmt.Errorf("unknown command %q; run ucd help", args[0])
	}
}

func parseEngine(action string, args []string) (invocation, error) {
	f := flags("engine " + action)
	engine := f.String("engine", "", "")
	if err := f.Parse(args); err != nil {
		return invocation{}, err
	}
	if f.NArg() != 0 {
		return invocation{}, errors.New("unexpected arguments")
	}
	if action == "ls" || action == "list" {
		if *engine != "" {
			return invocation{}, errors.New("engine list does not accept --engine")
		}
		return invocation{method: "engine.list", params: struct{}{}}, nil
	}
	if action != "status" && action != "start" && action != "stop" {
		return invocation{}, fmt.Errorf("unknown engine action %q", action)
	}
	if *engine == "" {
		return invocation{}, errors.New("--engine is required")
	}
	return invocation{method: "engine." + action, engine: *engine, params: struct{}{}}, nil
}

func parseProject(action string, args []string) (invocation, error) {
	if action == "import" {
		return parseImport(args)
	}
	f := flags("project " + action)
	id := f.String("id", "", "")
	engine := f.String("engine", "", "")
	file := f.String("file", "", "")
	f.StringVar(file, "f", "", "")
	removeVolumes := f.Bool("volumes", false, "")
	if err := f.Parse(args); err != nil {
		return invocation{}, err
	}
	if f.NArg() != 0 {
		return invocation{}, errors.New("unexpected arguments")
	}
	if action == "ls" || action == "list" {
		if *id != "" || *engine != "" || *file != "" || *removeVolumes {
			return invocation{}, errors.New("project list does not accept these options")
		}
		return invocation{method: "project.list", params: struct{}{}}, nil
	}
	if *id == "" {
		return invocation{}, errors.New("--id is required")
	}
	p := api.ProjectParams{ID: *id, Engine: *engine, RemoveVolumes: *removeVolumes}
	if action != "write" && *file != "" {
		return invocation{}, errors.New("--file is only valid for project write")
	}
	if *removeVolumes && action != "down" && action != "remove" {
		return invocation{}, errors.New("--volumes is only valid for project down or remove")
	}
	switch action {
	case "read":
		return invocation{method: "project." + action, engine: *engine, params: p}, nil
	case "write":
		if *engine == "" || *file == "" {
			return invocation{}, errors.New("project write requires --engine and --file")
		}
		data, err := readText(*file)
		if err != nil {
			return invocation{}, err
		}
		if *engine == "linux" && len(data) > 4<<20 {
			return invocation{}, errors.New("Linux Compose file exceeds 4 MiB")
		}
		p.YAML = data
		return invocation{method: "project.write", engine: *engine, params: p}, nil
	case "up", "start", "stop", "restart", "down", "build", "pull", "logs", "remove":
		p.Action = action
		return invocation{method: "project.action", engine: *engine, params: p}, nil
	default:
		return invocation{}, fmt.Errorf("unknown project action %q", action)
	}
}

func parseContainer(action string, args []string) (invocation, error) {
	f := flags("container " + action)
	engine := f.String("engine", "", "")
	id := f.String("id", "", "")
	tail := f.Int("tail", 200, "")
	force := f.Bool("force", false, "")
	if err := f.Parse(args); err != nil {
		return invocation{}, err
	}
	if f.NArg() != 0 {
		return invocation{}, errors.New("unexpected arguments")
	}
	if action == "ls" || action == "list" {
		return invocation{method: "container.list", engine: *engine, params: struct{}{}}, nil
	}
	if *engine == "" || *id == "" {
		return invocation{}, errors.New("--engine and --id are required")
	}
	p := api.ContainerParams{ID: *id, Tail: *tail, Force: *force}
	switch action {
	case "inspect", "logs", "stats":
		return invocation{method: "container." + action, engine: *engine, params: p}, nil
	case "start", "stop", "restart", "rm", "remove":
		if action == "rm" {
			action = "remove"
		}
		p.Action = action
		return invocation{method: "container.action", engine: *engine, params: p}, nil
	default:
		return invocation{}, fmt.Errorf("unknown container action %q", action)
	}
}

func parseImage(action string, args []string) (invocation, error) {
	f := flags("image " + action)
	engine := f.String("engine", "", "")
	id := f.String("id", "", "")
	reference := f.String("reference", "", "")
	if err := f.Parse(args); err != nil {
		return invocation{}, err
	}
	if f.NArg() != 0 {
		return invocation{}, errors.New("unexpected arguments")
	}
	if action == "ls" || action == "list" {
		return invocation{method: "image.list", engine: *engine, params: struct{}{}}, nil
	}
	if *engine == "" {
		return invocation{}, errors.New("--engine is required")
	}
	switch action {
	case "pull":
		if *reference == "" {
			return invocation{}, errors.New("--reference is required")
		}
		return invocation{method: "image.pull", engine: *engine, params: api.ImageParams{Reference: *reference}}, nil
	case "rm", "remove":
		if *id == "" {
			return invocation{}, errors.New("--id is required")
		}
		return invocation{method: "image.remove", engine: *engine, params: api.ImageParams{ID: *id}}, nil
	default:
		return invocation{}, fmt.Errorf("unknown image action %q", action)
	}
}

func parseVolume(action string, args []string) (invocation, error) {
	f := flags("volume " + action)
	engine := f.String("engine", "", "")
	name := f.String("name", "", "")
	if err := f.Parse(args); err != nil {
		return invocation{}, err
	}
	if f.NArg() != 0 {
		return invocation{}, errors.New("unexpected arguments")
	}
	if action == "ls" || action == "list" {
		return invocation{method: "volume.list", engine: *engine, params: struct{}{}}, nil
	}
	if *engine == "" || *name == "" {
		return invocation{}, errors.New("--engine and --name are required")
	}
	if action == "rm" {
		action = "remove"
	}
	if action != "remove" && action != "inspect" {
		return invocation{}, fmt.Errorf("unknown volume action %q", action)
	}
	return invocation{method: "volume." + action, engine: *engine, params: api.VolumeParams{Name: *name}}, nil
}

func parseSettings(action string, args []string) (invocation, error) {
	f := flags("settings " + action)
	file := f.String("file", "", "")
	if err := f.Parse(args); err != nil {
		return invocation{}, err
	}
	if f.NArg() != 0 {
		return invocation{}, errors.New("unexpected arguments")
	}
	if action == "show" {
		return invocation{method: "system.settings", params: struct{}{}}, nil
	}
	if action != "configure" {
		return invocation{}, fmt.Errorf("unknown settings action %q", action)
	}
	if *file == "" {
		return invocation{}, errors.New("--file is required and must contain a settings JSON object")
	}
	data, err := readText(*file)
	if err != nil {
		return invocation{}, err
	}
	var settings api.Settings
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&settings); err != nil {
		return invocation{}, fmt.Errorf("decode settings: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return invocation{}, errors.New("settings file must contain one JSON object")
	}
	return invocation{method: "system.configure", params: settings}, nil
}

func parseWSL(action string, args []string) (invocation, error) {
	f := flags("wsl " + action)
	cpus := f.Int("cpus", 0, "")
	memory := f.Int64("memory-mb", 0, "")
	disk := f.Int64("disk-gb", 0, "")
	if err := f.Parse(args); err != nil {
		return invocation{}, err
	}
	if f.NArg() != 0 {
		return invocation{}, errors.New("unexpected arguments")
	}
	if action == "status" {
		return invocation{method: "wsl.status", params: struct{}{}}, nil
	}
	if action != "configure" {
		return invocation{}, fmt.Errorf("unknown wsl action %q", action)
	}
	if *cpus <= 0 || *memory <= 0 || *disk <= 0 {
		return invocation{}, errors.New("--cpus, --memory-mb and --disk-gb must be positive")
	}
	return invocation{method: "wsl.configure", params: struct {
		CPUs     int   `json:"cpus"`
		MemoryMB int64 `json:"memoryMB"`
		DiskGB   int64 `json:"diskGB"`
	}{*cpus, *memory, *disk}}, nil
}

func parseOperator(action string, args []string) (invocation, error) {
	f := flags("operator " + action)
	account := f.String("account", "", "")
	if err := f.Parse(args); err != nil {
		return invocation{}, err
	}
	if f.NArg() != 0 {
		return invocation{}, errors.New("unexpected arguments")
	}
	if action == "ls" {
		action = "list"
	}
	if action != "list" && action != "add" && action != "remove" {
		return invocation{}, fmt.Errorf("unknown operator action %q", action)
	}
	if action != "list" && *account == "" {
		return invocation{}, errors.New("--account is required")
	}
	return invocation{method: "operator." + action, params: struct {
		Account string `json:"account,omitempty"`
	}{*account}}, nil
}

func parseShare(action string, args []string) (invocation, error) {
	f := flags("share " + action)
	name := f.String("name", "", "")
	path := f.String("path", "", "")
	if err := f.Parse(args); err != nil {
		return invocation{}, err
	}
	if f.NArg() != 0 {
		return invocation{}, errors.New("unexpected arguments")
	}
	if action == "ls" {
		action = "list"
	}
	if action != "list" && action != "create" && action != "remove" {
		return invocation{}, fmt.Errorf("unknown share action %q", action)
	}
	if action != "list" && *name == "" {
		return invocation{}, errors.New("--name is required")
	}
	if action == "create" && *path == "" {
		return invocation{}, errors.New("--path is required")
	}
	return invocation{method: "share." + action, params: struct {
		Name        string `json:"name,omitempty"`
		WindowsPath string `json:"windowsPath,omitempty"`
	}{*name, *path}}, nil
}

func readText(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > 8<<20 {
		return "", fmt.Errorf("%s exceeds 8 MiB", path)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s must contain UTF-8 text", path)
	}
	return string(data), nil
}
