package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

type dockerChildResult struct {
	Args         []string `json:"args"`
	Input        string   `json:"input"`
	ProgramFiles string   `json:"programFiles"`
	Path         string   `json:"path"`
}

func TestMain(m *testing.M) {
	if os.Getenv("UCD_TEST_DOCKER_CHILD") == "1" {
		if os.Getenv("UCD_TEST_CONSOLE_CHILD") == "1" {
			if !dockerTestConsole() {
				os.Exit(91)
			}
			if os.Getenv("UCD_TEST_CONSOLE_SIGNAL") == "1" {
				interrupt := make(chan os.Signal, 1)
				signal.Notify(interrupt, os.Interrupt)
				fmt.Fprintln(os.Stdout, "UCD_CHILD_READY")
				select {
				case <-interrupt:
					fmt.Fprintln(os.Stdout, "UCD_CHILD_INTERRUPT")
				case <-time.After(15 * time.Second):
					os.Exit(92)
				}
			}
			os.Exit(0)
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(90)
		}
		_ = json.NewEncoder(os.Stdout).Encode(dockerChildResult{Args: os.Args[1:], Input: string(data), ProgramFiles: os.Getenv("ProgramFiles"), Path: os.Getenv("PATH")})
		fmt.Fprint(os.Stderr, "child-stderr")
		if os.Getenv("UCD_TEST_DOCKER_FAIL") == "1" {
			os.Exit(37)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeDockerInstall(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "cli-plugins")
	if err := os.Mkdir(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"docker-compose.exe", "docker-buildx.exe"} {
		if err := os.WriteFile(filepath.Join(pluginDir, name), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-credential-ucd.exe"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-cli.exe"), contents, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDockerDispatcherUsesSharedBundle(t *testing.T) {
	install := fakeDockerInstall(t)
	t.Setenv("UCD_TEST_DOCKER_CHILD", "1")
	for _, executable := range []string{
		filepath.Join(install, "ucd.exe"),
		filepath.Join(install, "docker-command", "docker.exe"),
		filepath.Join(install, "docker-command", "DOCKER.EXE"),
	} {
		t.Run(filepath.Base(executable), func(t *testing.T) {
			config := t.TempDir()
			var out, stderr bytes.Buffer
			code := runDockerProgram(context.Background(), dockerInstallDirectory(executable), linuxDockerHost,
				[]string{"--config", config, "version"}, strings.NewReader(""), &out, &stderr)
			if code != 0 {
				t.Fatalf("shared Docker client exited %d: %s", code, stderr.String())
			}
			var child dockerChildResult
			if err := json.Unmarshal(out.Bytes(), &child); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(child.Path, install+string(os.PathListSeparator)) {
				t.Fatalf("shared credential helper is missing from PATH: %q", child.Path)
			}
			data, err := os.ReadFile(filepath.Join(config, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			var registered struct {
				PluginDirs []string `json:"cliPluginsExtraDirs"`
			}
			if err := json.Unmarshal(data, &registered); err != nil {
				t.Fatal(err)
			}
			if len(registered.PluginDirs) == 0 || registered.PluginDirs[0] != filepath.Join(install, "cli-plugins") {
				t.Fatalf("shared plugin directory was not registered: %q", registered.PluginDirs)
			}
		})
	}
}

func TestDockerEntrypointArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"Linux default", []string{"ps"}, []string{"--engine", "linux", "ps"}},
		{"explicit Linux", []string{"--engine", "linux", "compose", "up"}, []string{"--engine", "linux", "compose", "up"}},
		{"explicit Windows", []string{"--engine", "windows", "ps"}, []string{"--engine", "windows", "ps"}},
		{"Windows assignment", []string{"--engine=windows", "ps"}, []string{"--engine", "windows", "ps"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dockerEntrypointArguments(tt.args)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("dockerEntrypointArguments(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
	if _, err := dockerEntrypointArguments([]string{"--engine"}); err == nil {
		t.Fatal("--engine without a value was accepted")
	}
}

func TestDockerIntroductionIsLimitedToHelp(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"no args", nil, true},
		{"top-level long help", []string{"--help"}, true},
		{"top-level short help", []string{"-h"}, true},
		{"Docker help command", []string{"help"}, true},
		{"Docker command help", []string{"help", "run"}, true},
		{"Compose", []string{"compose"}, true},
		{"Compose help", []string{"compose", "--help"}, true},
		{"Buildx", []string{"buildx"}, true},
		{"Buildx help", []string{"buildx", "-h"}, true},
		{"global options", []string{"--config", "some directory", "--debug", "compose"}, true},
		{"normal command", []string{"ps"}, false},
		{"command help", []string{"compose", "up", "--help"}, false},
		{"container argument", []string{"run", "image", "--help"}, false},
		{"exec argument", []string{"exec", "container", "app", "--help"}, false},
		{"Compose exec argument", []string{"compose", "exec", "service", "app", "--help"}, false},
		{"Compose JSON", []string{"compose", "ls", "--format", "json"}, false},
		{"Buildx output", []string{"buildx", "version"}, false},
		{"version", []string{"--version"}, false},
		{"help with extra argument", []string{"--help", "ps"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			writeDockerIntroduction(tt.args, &output)
			got := output.Len() != 0
			if got != tt.want {
				t.Fatalf("hint displayed=%v, want %v; output %q", got, tt.want, output.String())
			}
			if got && !strings.Contains(output.String(), "docker --engine windows") {
				t.Fatalf("help hint does not explain Windows selection: %q", output.String())
			}
		})
	}
}

func TestDockerEngineSelectorErrorKeepsHintOutputEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunDocker(context.Background(), nil, []string{"--engine"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--engine requires linux or windows") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestDockerArguments(t *testing.T) {
	tests := []struct {
		args    []string
		host    string
		rest    []string
		wantErr bool
	}{
		{[]string{"--engine", "linux", "run", "--name", "with space", "image"}, linuxDockerHost, []string{"run", "--name", "with space", "image"}, false},
		{[]string{"--engine=windows", "compose", "up", "-d"}, windowsDockerHost, []string{"compose", "up", "-d"}, false},
		{[]string{"--engine", "other", "ps"}, "", nil, true},
		{[]string{"ps"}, "", nil, true},
		{[]string{"--engine"}, "", nil, true},
	}
	for _, tt := range tests {
		host, args, err := dockerArguments(tt.args)
		if (err != nil) != tt.wantErr || host != tt.host || !reflect.DeepEqual(args, tt.rest) {
			t.Errorf("dockerArguments(%q) = %q, %q, %v", tt.args, host, args, err)
		}
	}
}

func TestDockerEndpointOverrideParsingOnlyBlocksGlobalFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--host", "tcp://example.test:2375", "ps"},
		{"-Htcp://example.test:2375", "ps"},
		{"--context", "other", "ps"},
		{"-cother", "ps"},
	} {
		if err := rejectDockerEndpointOverrides(args); err == nil {
			t.Errorf("endpoint override %q was accepted", args)
		}
	}
	if err := rejectDockerEndpointOverrides([]string{"commit", "-c", "CMD /bin/sh"}); err != nil {
		t.Fatalf("valid docker commit flag was rejected: %v", err)
	}
}

func TestDockerEnvironmentRemovesExternalDaemonOverrides(t *testing.T) {
	got := dockerEnvironment([]string{
		"DOCKER_HOST=tcp://external.example:2375",
		"docker_context=external",
		"BuildX_Builder=remote",
		"BUILDKIT_HOST=tcp://buildkit.example:1234",
		"DOCKER_API_VERSION=1.45",
		"PATH=C:\\Windows",
	})
	want := []string{"DOCKER_API_VERSION=1.45", "PATH=C:\\Windows"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Docker child environment = %#v, want %#v", got, want)
	}
}

func TestDockerPassesArgumentsStreamsAndExitCode(t *testing.T) {
	install := fakeDockerInstall(t)
	config := t.TempDir()
	t.Setenv("UCD_TEST_DOCKER_CHILD", "1")
	programFiles := os.Getenv("ProgramFiles")
	args := []string{"--config", config, "run", "-i", "--name", `a b\`, "image", "quote\" and spaces", "line\nbreak"}
	var out, stderr bytes.Buffer
	code := runDockerProgram(context.Background(), install, linuxDockerHost, args, strings.NewReader("stdin payload\n"), &out, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	var result dockerChildResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	wantArgs := append([]string{"--host", linuxDockerHost}, args...)
	if !reflect.DeepEqual(result.Args, wantArgs) || result.Input != "stdin payload\n" || result.ProgramFiles != programFiles {
		t.Fatalf("unexpected child result: %#v", result)
	}
	if !strings.HasPrefix(result.Path, install+string(os.PathListSeparator)) {
		t.Fatalf("credential helper directory is missing from child PATH: %q", result.Path)
	}
	if !strings.Contains(stderr.String(), "registered bundled") || !strings.Contains(stderr.String(), "child-stderr") {
		t.Fatal(stderr.String())
	}
	stderr.Reset()
	out.Reset()
	t.Setenv("UCD_TEST_DOCKER_FAIL", "1")
	code = runDockerProgram(context.Background(), install, windowsDockerHost, args, strings.NewReader(""), &out, &stderr)
	if code != 37 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "registered bundled") {
		t.Fatalf("registration should be idempotent: %s", stderr.String())
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Args[1] != windowsDockerHost {
		t.Fatalf("Windows host = %q", result.Args[1])
	}
}

func TestUCDClientConfigSelectsSharedCredentialHelper(t *testing.T) {
	install := fakeDockerInstall(t)
	ordinaryConfig := filepath.Join(t.TempDir(), "docker")
	if err := os.Mkdir(ordinaryConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	ordinaryFile := filepath.Join(ordinaryConfig, "config.json")
	ordinary := []byte(`{"currentContext":"docker-desktop","credsStore":"desktop"}`)
	if err := os.WriteFile(ordinaryFile, ordinary, 0o600); err != nil {
		t.Fatal(err)
	}
	ucdConfig := filepath.Join(t.TempDir(), "UCD", "docker")
	changed, err := installUCDClientConfig(install, ucdConfig)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(filepath.Join(ucdConfig, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["credsStore"] != "ucd" {
		t.Fatalf("UCD credential store = %#v", config["credsStore"])
	}
	if _, err := os.Stat(config["cliPluginsExtraDirs"].([]any)[0].(string)); err != nil {
		t.Fatalf("plugin directory was not registered: %v", err)
	}
	after, err := os.ReadFile(ordinaryFile)
	if err != nil || !bytes.Equal(after, ordinary) {
		t.Fatalf("ordinary Docker config changed: %v", err)
	}
	changed, err = installUCDClientConfig(install, ucdConfig)
	if err != nil || changed {
		t.Fatalf("repeat registration changed=%v err=%v", changed, err)
	}
}

func TestExplicitDockerConfigDoesNotOverrideCredentialStore(t *testing.T) {
	install := fakeDockerInstall(t)
	configDir := t.TempDir()
	original := []byte(`{"credsStore":"desktop","proxies":{"default":{"httpProxy":"http://existing:8080","ftpProxy":"ftp://existing:21","noProxy":"localhost"},"named":{"httpProxy":"http://named:8080"}}}`)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UCD_TEST_DOCKER_CHILD", "1")
	proxy := api.ProxySettings{HTTP: "http://managed:8080", SOCKS5: "socks5://managed:1080", NoProxy: "example.test"}
	var out, stderr bytes.Buffer
	code := runDockerProgramWithProxy(context.Background(), install, linuxDockerHost,
		[]string{"--config", configDir, "version"}, &proxy, strings.NewReader(""), &out, &stderr)
	if code != 0 {
		t.Fatalf("Docker exit %d, stderr %s", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["credsStore"]) != `"desktop"` {
		t.Fatalf("explicit credential store changed: %s", got["credsStore"])
	}
	var originalConfig, currentConfig map[string]any
	if err := json.Unmarshal(original, &originalConfig); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &currentConfig); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(currentConfig["proxies"], originalConfig["proxies"]) {
		t.Fatalf("explicit Docker config proxy settings changed: got %#v, want %#v", currentConfig["proxies"], originalConfig["proxies"])
	}
}

func TestUCDClientConfigMapsAndClearsOnlyContainerProxyFields(t *testing.T) {
	install := fakeDockerInstall(t)
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	original := []byte(`{"proxies":{"default":{"httpProxy":"http://old:8080","ftpProxy":"ftp://keep:21","noProxy":"old"},"named":{"httpProxy":"http://named:8080"}}}`)
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	settings := api.ProxySettings{
		HTTP:    "http://proxy.example:8080",
		HTTPS:   "https://secure.example:8443",
		SOCKS5:  "socks5://socks.example:1080",
		NoProxy: "localhost,.internal",
	}
	changed, err := installUCDClientConfigWithProxy(install, configDir, &settings)
	if err != nil || !changed {
		t.Fatalf("apply proxy settings changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Proxies map[string]map[string]string `json:"proxies"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	wantDefaults := map[string]string{
		"httpProxy": "http://proxy.example:8080", "httpsProxy": "https://secure.example:8443",
		"allProxy": "socks5://socks.example:1080", "noProxy": "localhost,.internal", "ftpProxy": "ftp://keep:21",
	}
	if !reflect.DeepEqual(config.Proxies["default"], wantDefaults) {
		t.Fatalf("default proxy settings = %#v, want %#v", config.Proxies["default"], wantDefaults)
	}
	if !reflect.DeepEqual(config.Proxies["named"], map[string]string{"httpProxy": "http://named:8080"}) {
		t.Fatalf("named proxy profile changed: %#v", config.Proxies["named"])
	}
	changed, err = installUCDClientConfigWithProxy(install, configDir, &settings)
	if err != nil || changed {
		t.Fatalf("repeat proxy settings changed=%v err=%v", changed, err)
	}
	changed, err = installUCDClientConfigWithProxy(install, configDir, &api.ProxySettings{})
	if err != nil || !changed {
		t.Fatalf("clear proxy settings changed=%v err=%v", changed, err)
	}
	data, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Proxies["default"], map[string]string{"ftpProxy": "ftp://keep:21"}) {
		t.Fatalf("clear removed unrelated default proxy keys: %#v", config.Proxies["default"])
	}
	if !reflect.DeepEqual(config.Proxies["named"], map[string]string{"httpProxy": "http://named:8080"}) {
		t.Fatalf("clear changed named proxy profile: %#v", config.Proxies["named"])
	}
}

func TestUCDClientConfigRejectsConflictingCredentialStore(t *testing.T) {
	install := fakeDockerInstall(t)
	configDir := t.TempDir()
	original := []byte(`{"credsStore":"desktop"}`)
	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := installUCDClientConfig(install, configDir)
	if err == nil || changed || !strings.Contains(err.Error(), "different Docker credential store") {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("conflicting UCD config was modified: %v", err)
	}
}

func TestDockerRegistrationPreservesConfiguration(t *testing.T) {
	install := fakeDockerInstall(t)
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	original := `{"currentContext":"existing", "auths":{"example.test":{"auth":"fixture"}}, "credHelpers":{"example.test":"fixture"}, "unknown":{"value":[1,2]}, "cliPluginsExtraDirs":["prior"]}`
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := installPlugins(install, configDir)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if err := json.Unmarshal([]byte(original), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"currentContext", "auths", "credHelpers", "unknown"} {
		if !reflect.DeepEqual(before[key], after[key]) {
			t.Errorf("%s changed", key)
		}
	}
	expected := []any{filepath.Join(install, "cli-plugins"), "prior"}
	if !reflect.DeepEqual(after["cliPluginsExtraDirs"], expected) {
		t.Fatalf("plugin order = %#v", after["cliPluginsExtraDirs"])
	}
	changed, err = installPlugins(install, configDir)
	if err != nil || changed {
		t.Fatalf("second registration changed=%v err=%v", changed, err)
	}
	again, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("idempotent registration changed the file: %v", err)
	}
	// A previously registered directory may have been moved behind a different
	// plugin provider. Restore bundled-plugin precedence without duplicating it.
	after["cliPluginsExtraDirs"] = []string{"prior", filepath.Join(install, "cli-plugins"), "last"}
	data, err = json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	changed, err = installPlugins(install, configDir)
	if err != nil || !changed {
		t.Fatalf("restore precedence: changed=%v err=%v", changed, err)
	}
	data, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after["cliPluginsExtraDirs"], []any{filepath.Join(install, "cli-plugins"), "prior", "last"}) {
		t.Fatalf("plugin precedence was not restored: %#v", after["cliPluginsExtraDirs"])
	}
}

func TestDockerRegistrationErrorsAreActionable(t *testing.T) {
	install := fakeDockerInstall(t)
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.json")
	original := []byte("{not-json")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := installPlugins(install, configDir)
	if changed || err == nil || !strings.Contains(err.Error(), "parse Docker config") {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	after, readErr := os.ReadFile(configPath)
	if readErr != nil || !bytes.Equal(after, original) {
		t.Fatalf("config modified after error: %v", readErr)
	}
	if err := os.Remove(filepath.Join(install, "cli-plugins", "docker-compose.exe")); err != nil {
		t.Fatal(err)
	}
	_, err = installPlugins(install, configDir)
	if err == nil || !strings.Contains(err.Error(), "repair the UCD installation") {
		t.Fatalf("missing plugin error: %v", err)
	}
}

func TestDockerStartAndCancellationErrors(t *testing.T) {
	install := fakeDockerInstall(t)
	configDir := t.TempDir()
	if err := os.Remove(filepath.Join(install, "docker-cli.exe")); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := runDockerProgram(context.Background(), install, linuxDockerHost, []string{"--config", configDir, "ps"}, strings.NewReader(""), &out, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "ucd: start") {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stderr.Reset()
	code = runDockerProgram(ctx, install, linuxDockerHost, nil, strings.NewReader(""), &out, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), context.Canceled.Error()) {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal(ctx.Err())
	}
}
