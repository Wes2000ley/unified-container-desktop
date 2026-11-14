package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	cliflags "github.com/docker/cli/cli/flags"
	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/local"
	"github.com/spf13/pflag"
)

const (
	linuxDockerHost   = "npipe:////./pipe/UCDLinuxDocker"
	windowsDockerHost = "npipe:////./pipe/UCDDocker"
)

// RunDocker delegates the standard Docker CLI to UCD's authenticated broker.
// It targets the Linux engine by default; an initial --engine windows selects
// the Windows engine.
func RunDocker(ctx context.Context, caller api.Caller, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	forwarded, err := dockerEntrypointArguments(args)
	if err != nil {
		fmt.Fprintln(stderr, "docker:", err)
		return 1
	}
	return runDocker(ctx, caller, forwarded, stdin, stdout, stderr)
}

const dockerIntroduction = `Unified Container Desktop (UCD)
Linux and Windows containers, side by side. One app, two shared machine-wide engines.

Linux is the default; select Windows with docker --engine windows COMMAND.
Compose and Buildx use the selected engine, for example:
  docker compose up -d
  docker --engine windows compose up -d

UCD manages engine access and shares registry sign-ins with its desktop and web GUI.
Use ucd --help for app settings, file sharing, and engine management.

`

func writeDockerIntroduction(args []string, stdout io.Writer) {
	flags := pflag.NewFlagSet("docker", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.SetInterspersed(false)
	cliflags.NewClientOptions().InstallFlags(flags)
	flags.BoolP("help", "h", false, "")
	if err := flags.Parse(args); err != nil {
		return
	}
	command := flags.Args()
	show := len(command) == 0 || command[0] == "help"
	if len(command) > 0 && (command[0] == "compose" || command[0] == "buildx") {
		show = len(command) == 1 || len(command) == 2 &&
			(command[1] == "--help" || command[1] == "-h" || command[1] == "help")
	}
	if show {
		fmt.Fprint(stdout, dockerIntroduction)
	}
}

func dockerEntrypointArguments(args []string) ([]string, error) {
	engine := "linux"
	if len(args) != 0 {
		switch {
		case args[0] == "--engine":
			if len(args) < 2 {
				return nil, errors.New("--engine requires linux or windows")
			}
			engine, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--engine="):
			engine, args = strings.TrimPrefix(args[0], "--engine="), args[1:]
		}
	}
	return append([]string{"--engine", engine}, args...), nil
}

// runDocker delegates all Docker arguments and I/O to the bundled Docker CLI.
func runDocker(ctx context.Context, caller api.Caller, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, dockerIntroduction)
		fmt.Fprintln(stdout, "Usage: ucd docker --engine linux|windows [DOCKER OPTIONS] COMMAND [ARG...]")
		fmt.Fprintln(stdout, "Runs the bundled Docker CLI for the selected engine, including Compose and Buildx.")
		return 0
	}
	host, args, err := dockerArguments(args)
	if err != nil {
		fmt.Fprintln(stderr, "ucd:", err)
		return 1
	}
	writeDockerIntroduction(args, stdout)
	engine := "windows"
	if host == linuxDockerHost {
		engine = "linux"
	}
	client, ok := caller.(*local.Client)
	if !ok {
		fmt.Fprintln(stderr, "ucd: authenticated Docker broker is unavailable")
		return 1
	}
	var system struct {
		Settings api.Settings `json:"settings"`
	}
	if err := client.CallInto(ctx, api.Request{Method: "system.settings"}, &system); err != nil {
		fmt.Fprintln(stderr, "ucd: read Docker proxy settings:", err)
		return 1
	}
	relay, err := client.DockerRelay(ctx, engine)
	if err != nil {
		fmt.Fprintln(stderr, "ucd: start authenticated Docker broker:", err)
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "ucd: locate installed Docker client:", err)
		_ = relay.Close()
		return 1
	}
	code := runDockerProgramWithProxy(ctx, dockerInstallDirectory(executable), relay.Endpoint(), args, &system.Settings.ContainersProxy, stdin, stdout, stderr)
	if err := relay.Close(); err != nil {
		fmt.Fprintln(stderr, "ucd: close authenticated Docker broker:", err)
		if code == 0 {
			return 1
		}
	}
	return code
}

func dockerInstallDirectory(executable string) string {
	directory := filepath.Dir(executable)
	name := filepath.Base(executable)
	// The docker dispatcher lives in docker-command; its shared client, plugins
	// and credential helper live beside ucd.exe in the installation root.
	if strings.EqualFold(name, "docker.exe") || strings.EqualFold(name, "docker") {
		return filepath.Dir(directory)
	}
	return directory
}

func dockerArguments(args []string) (string, []string, error) {
	var engine string
	switch {
	case len(args) >= 2 && args[0] == "--engine":
		engine, args = args[1], args[2:]
	case len(args) >= 1 && strings.HasPrefix(args[0], "--engine="):
		engine, args = strings.TrimPrefix(args[0], "--engine="), args[1:]
	default:
		return "", nil, errors.New("docker requires --engine linux or windows before Docker arguments")
	}
	if err := rejectDockerEndpointOverrides(args); err != nil {
		return "", nil, err
	}
	host := ""
	switch engine {
	case "linux":
		host = linuxDockerHost
	case "windows":
		host = windowsDockerHost
	default:
		return "", nil, errors.New("docker engine must be linux or windows")
	}
	return host, args, nil
}

func rejectDockerEndpointOverrides(args []string) error {
	flags := pflag.NewFlagSet("docker", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.SetInterspersed(false)
	cliflags.NewClientOptions().InstallFlags(flags)
	_ = flags.Parse(args)
	for _, name := range []string{"host", "context"} {
		if flag := flags.Lookup(name); flag != nil && flag.Changed {
			return errors.New("Docker host and context are managed by UCD; remove --host/-H or --context/-c")
		}
	}
	return nil
}

func runDockerProgram(ctx context.Context, installDir, host string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runDockerProgramWithProxy(ctx, installDir, host, args, nil, stdin, stdout, stderr)
}

func runDockerProgramWithProxy(ctx context.Context, installDir, host string, args []string, containersProxy *api.ProxySettings, stdin io.Reader, stdout, stderr io.Writer) int {
	if err := ctx.Err(); err != nil {
		fmt.Fprintln(stderr, "ucd: Docker command:", err)
		return 1
	}
	// Use Docker's own flag definitions to honor an explicit --config. Invalid
	// flags are left to Docker to report, without changing configuration.
	opts := cliflags.NewClientOptions()
	f := pflag.NewFlagSet("docker", pflag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.SetInterspersed(false)
	opts.InstallFlags(f)
	f.BoolP("help", "h", false, "")
	f.BoolP("version", "v", false, "")
	parseErr := f.Parse(args)
	configFlag := f.Lookup("config")
	explicitConfig := configFlag != nil && configFlag.Changed
	configDir := opts.ConfigDir
	var err error
	if !explicitConfig {
		configDir, err = ucdDockerConfigDir()
		if err != nil {
			fmt.Fprintln(stderr, "ucd: resolve UCD Docker client configuration:", err)
			return 1
		}
	}
	forwarded := []string{"--host", host}
	if !explicitConfig {
		forwarded = append(forwarded, "--config", configDir)
	}
	forwarded = append(forwarded, args...)
	if parseErr == nil {
		var changed bool
		var err error
		if explicitConfig {
			changed, err = installPlugins(installDir, configDir)
		} else if containersProxy != nil {
			changed, err = installUCDClientConfigWithProxy(installDir, configDir, containersProxy)
		} else {
			changed, err = installUCDClientConfig(installDir, configDir)
		}
		if err != nil {
			fmt.Fprintln(stderr, "ucd: register Docker plugins:", err)
			return 1
		}
		if changed {
			fmt.Fprintf(stderr, "ucd: registered bundled Compose and Buildx in %s\n", filepath.Join(configDir, "config.json"))
		}
	}
	docker := filepath.Join(installDir, "docker-cli.exe")
	cmd := exec.CommandContext(ctx, docker, forwarded...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = withDockerHelperPath(dockerEnvironment(os.Environ()), installDir)
	// The child stays in our console process group, so Ctrl+C reaches it
	// directly and Docker can perform its normal signal handling and cleanup.
	err = cmd.Run()
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	fmt.Fprintf(stderr, "ucd: start %s: %v\n", docker, err)
	return 1
}

func dockerEnvironment(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (strings.EqualFold(key, "DOCKER_HOST") || strings.EqualFold(key, "DOCKER_CONTEXT") ||
			strings.EqualFold(key, "BUILDX_BUILDER") || strings.EqualFold(key, "BUILDKIT_HOST")) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func withDockerHelperPath(env []string, installDir string) []string {
	result := append([]string(nil), env...)
	for i, entry := range result {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, "PATH") {
			result[i] = key + "=" + installDir + string(os.PathListSeparator) + value
			return result
		}
	}
	return append(result, "PATH="+installDir)
}

func ucdDockerConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the current user's UCD Docker settings: %w", err)
	}
	return filepath.Join(base, "UnifiedContainerDesktop", "docker"), nil
}

// installPlugins registers the bundled plugins in a selected Docker client
// configuration while preserving its credential store and existing directory order.
func installPlugins(installDir, configDir string) (bool, error) {
	return updateDockerConfig(installDir, configDir, false, nil)
}

func installUCDClientConfig(installDir, configDir string) (bool, error) {
	return installUCDClientConfigWithProxy(installDir, configDir, nil)
}

func installUCDClientConfigWithProxy(installDir, configDir string, containersProxy *api.ProxySettings) (bool, error) {
	helperPath := filepath.Join(installDir, "docker-credential-ucd.exe")
	helperInfo, err := os.Stat(helperPath)
	if err != nil {
		return false, fmt.Errorf("bundled registry credential helper: %w; repair the UCD installation", err)
	}
	if !helperInfo.Mode().IsRegular() {
		return false, errors.New("bundled registry credential helper is not a regular file; repair the UCD installation")
	}
	if containersProxy != nil {
		if err := api.ValidateProxySettings("containers", *containersProxy); err != nil {
			return false, err
		}
	}
	return updateDockerConfig(installDir, configDir, true, containersProxy)
}

func updateDockerConfig(installDir, configDir string, configureCredentialStore bool, containersProxy *api.ProxySettings) (bool, error) {
	var err error
	configDir, err = filepath.Abs(configDir)
	if err != nil {
		return false, fmt.Errorf("resolve Docker config directory: %w", err)
	}
	pluginDir, err := filepath.Abs(filepath.Join(installDir, "cli-plugins"))
	if err != nil {
		return false, fmt.Errorf("resolve bundled plugins: %w", err)
	}
	for _, name := range []string{"docker-compose.exe", "docker-buildx.exe"} {
		info, err := os.Stat(filepath.Join(pluginDir, name))
		if err != nil {
			return false, fmt.Errorf("bundled plugin %s: %w; repair the UCD installation", name, err)
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("bundled plugin %s is not a regular file; repair the UCD installation", name)
		}
	}
	configPath := filepath.Join(configDir, "config.json")
	// Docker follows symlinks when saving its configuration; preserve that
	// behavior instead of replacing an existing link.
	if _, err := os.Lstat(configPath); err == nil {
		resolved, err := filepath.EvalSymlinks(configPath)
		if err != nil {
			return false, fmt.Errorf("resolve Docker config: %w", err)
		}
		configPath = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect Docker config: %w", err)
	}
	data, err := os.ReadFile(configPath)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read Docker config: %w", err)
	}
	config := map[string]json.RawMessage{}
	if len(data) != 0 {
		if err := json.Unmarshal(data, &config); err != nil {
			return false, fmt.Errorf("parse Docker config: %w", err)
		}
		if config == nil {
			return false, errors.New("Docker config must be a JSON object")
		}
	}
	var dirs []string
	if raw, ok := config["cliPluginsExtraDirs"]; ok {
		if err := json.Unmarshal(raw, &dirs); err != nil {
			return false, fmt.Errorf("parse Docker cliPluginsExtraDirs: %w", err)
		}
	}
	ordered := []string{pluginDir}
	for _, dir := range dirs {
		if !strings.EqualFold(filepath.Clean(dir), filepath.Clean(pluginDir)) {
			ordered = append(ordered, dir)
		}
	}
	changed := !slices.Equal(dirs, ordered)
	if configureCredentialStore {
		store := ""
		if raw, ok := config["credsStore"]; ok {
			if err := json.Unmarshal(raw, &store); err != nil {
				return false, fmt.Errorf("parse Docker credential store: %w", err)
			}
		}
		if store != "" && store != "ucd" {
			return false, errors.New("UCD's client config already selects a different Docker credential store")
		}
		if store == "" {
			rawStore, err := json.Marshal("ucd")
			if err != nil {
				return false, fmt.Errorf("encode Docker credential store: %w", err)
			}
			config["credsStore"] = rawStore
			changed = true
		}
	}
	proxyChanged, err := applyContainerProxy(config, containersProxy)
	if err != nil {
		return false, err
	}
	changed = changed || proxyChanged
	if !changed {
		return false, nil
	}
	dirs = ordered
	rawDirs, err := json.Marshal(dirs)
	if err != nil {
		return false, fmt.Errorf("encode Docker plugin directories: %w", err)
	}
	config["cliPluginsExtraDirs"] = rawDirs
	updated, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode Docker config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return false, fmt.Errorf("create Docker config directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(configPath), ".ucd-config-*")
	if err != nil {
		return false, fmt.Errorf("create temporary Docker config: %w", err)
	}
	defer os.Remove(tmp.Name())
	source := ""
	if exists {
		source = configPath
	}
	if err := protectConfigCopy(source, tmp.Name()); err != nil {
		tmp.Close()
		return false, fmt.Errorf("protect temporary Docker config: %w", err)
	}
	if _, err = tmp.Write(append(updated, '\n')); err != nil {
		tmp.Close()
		return false, fmt.Errorf("write Docker config: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return false, fmt.Errorf("close Docker config: %w", err)
	}
	if err := replaceDockerConfig(tmp.Name(), configPath, exists); err != nil {
		return false, fmt.Errorf("replace Docker config: %w", err)
	}
	return true, nil
}

func applyContainerProxy(config map[string]json.RawMessage, settings *api.ProxySettings) (bool, error) {
	if settings == nil {
		return false, nil
	}
	proxies := map[string]json.RawMessage{}
	if raw, ok := config["proxies"]; ok {
		if err := json.Unmarshal(raw, &proxies); err != nil {
			return false, fmt.Errorf("parse Docker proxy settings: %w", err)
		}
		if proxies == nil {
			return false, errors.New("Docker proxy settings must be a JSON object")
		}
	}
	defaults := map[string]json.RawMessage{}
	if raw, ok := proxies["default"]; ok {
		if err := json.Unmarshal(raw, &defaults); err != nil {
			return false, fmt.Errorf("parse Docker default proxy settings: %w", err)
		}
		if defaults == nil {
			return false, errors.New("Docker default proxy settings must be a JSON object")
		}
	}
	managed := []struct {
		name  string
		value string
	}{
		{"httpProxy", settings.HTTP},
		{"httpsProxy", settings.HTTPS},
		{"allProxy", settings.SOCKS5},
		{"noProxy", settings.NoProxy},
	}
	changed := false
	for _, item := range managed {
		current, exists := defaults[item.name]
		if item.value == "" {
			if exists {
				delete(defaults, item.name)
				changed = true
			}
			continue
		}
		var currentValue string
		if exists {
			if err := json.Unmarshal(current, &currentValue); err != nil {
				return false, fmt.Errorf("parse Docker %s: %w", item.name, err)
			}
		}
		if !exists || currentValue != item.value {
			encoded, err := json.Marshal(item.value)
			if err != nil {
				return false, fmt.Errorf("encode Docker %s: %w", item.name, err)
			}
			defaults[item.name] = encoded
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if len(defaults) == 0 {
		delete(proxies, "default")
	} else {
		encoded, err := json.Marshal(defaults)
		if err != nil {
			return false, fmt.Errorf("encode Docker default proxy settings: %w", err)
		}
		proxies["default"] = encoded
	}
	if len(proxies) == 0 {
		delete(config, "proxies")
	} else {
		encoded, err := json.Marshal(proxies)
		if err != nil {
			return false, fmt.Errorf("encode Docker proxy settings: %w", err)
		}
		config["proxies"] = encoded
	}
	return true, nil
}

func runPlugins(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "install" {
		fmt.Fprintln(stderr, "ucd: usage: ucd plugins install")
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "ucd: locate bundled plugins:", err)
		return 1
	}
	configDir, err := ucdDockerConfigDir()
	if err != nil {
		fmt.Fprintln(stderr, "ucd: resolve UCD Docker client configuration:", err)
		return 1
	}
	_, err = installUCDClientConfig(filepath.Dir(executable), configDir)
	if err != nil {
		fmt.Fprintln(stderr, "ucd: install Docker plugins:", err)
		return 1
	}
	fmt.Fprintln(stdout, "Bundled plugins and shared registry credentials configured for UCD's Docker client.")
	return 0
}
