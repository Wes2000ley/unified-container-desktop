//go:build windows

package platform

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	profile := filepath.Join(root, "service-profile")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	return &Manager{
		settings: api.Settings{DataRoot: root, WSL: api.WSLSettings{Distro: "UCD-Linux", ServiceAccount: "UcdService", ProfileDirectory: profile, InstallDirectory: filepath.Join(root, "wsl")}},
		state:    wslState{CPUs: 4, MemoryMB: 4096, DiskGB: 128},
		events:   &api.EventBus{},
	}
}

func TestConfigurePersistsLimitsAndMarksResize(t *testing.T) {
	m := testManager(t)
	result, err := m.configure(6, 8192, 256)
	if err != nil {
		t.Fatal(err)
	}
	state := result.(wslState)
	if !state.RestartRequired || !state.DiskResizePending || state.DiskGB != 256 {
		t.Fatalf("incorrect state: %+v", state)
	}
	config, err := os.ReadFile(filepath.Join(m.settings.WSL.ProfileDirectory, ".wslconfig"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"processors=6", "memory=8192MB", "defaultVhdSize=256GB", "networkingMode=mirrored", "dnsTunneling=true", "hostAddressLoopback=true", "telemetry=false", "[experimental]\nautoMemoryReclaim=gradual", "[general]\ninstanceIdleTimeout=-1"} {
		if !strings.Contains(string(config), want) {
			t.Errorf("missing %q in configuration", want)
		}
	}
	m2 := testManager(t)
	m2.settings.DataRoot = m.settings.DataRoot
	if err := m2.loadState(); err != nil {
		t.Fatal(err)
	}
	if m2.state.CPUs != 6 || !m2.state.DiskResizePending {
		t.Fatalf("state was not persisted: %+v", m2.state)
	}
	if _, err := m.configure(6, 8192, 128); err == nil {
		t.Fatal("accepted disk shrink")
	}
	if _, err := m.configure(0, 8192, 256); err == nil {
		t.Fatal("accepted zero CPUs")
	}
}

func TestStartupConfigUpgradeRecordsPendingRestartOnce(t *testing.T) {
	m := testManager(t)
	path := filepath.Join(m.settings.WSL.ProfileDirectory, ".wslconfig")
	if err := os.WriteFile(path, []byte("[wsl2]\nnetworkingMode=mirrored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.writeStartupConfig(true); err != nil {
		t.Fatal(err)
	}
	if !m.state.RestartRequired || m.state.ChangedAt.IsZero() {
		t.Fatalf("upgrade not pending: %+v", m.state)
	}
	changed := m.state.ChangedAt
	if err := m.writeStartupConfig(true); err != nil {
		t.Fatal(err)
	}
	if m.state.ChangedAt != changed {
		t.Fatal("unchanged startup config moved pending timestamp")
	}
	config, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(config), "hostAddressLoopback=true") {
		t.Fatalf("config=%s error=%v", config, err)
	}

	fresh := testManager(t)
	if err := fresh.writeStartupConfig(false); err != nil {
		t.Fatal(err)
	}
	if fresh.state.RestartRequired {
		t.Fatal("new installation requires unnecessary restart")
	}
}

func TestRestartStatusRequiresNewKernelBoot(t *testing.T) {
	for _, restarted := range []bool{false, true} {
		m := testManager(t)
		m.state.RestartRequired = true
		m.state.ChangedAt = time.Now().Add(-time.Minute).UTC()
		boot := m.state.ChangedAt.Add(-time.Hour)
		if restarted {
			boot = m.state.ChangedAt.Add(30 * time.Second)
		}
		m.runCommand = func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
			if !reflect.DeepEqual(args[len(args)-2:], []string{"/bin/cat", "/proc/stat"}) {
				t.Fatalf("unexpected command %v", args)
			}
			return []byte(fmt.Sprintf("cpu 0 0 0\nbtime %d\n", boot.Unix())), nil
		}
		if err := m.clearRestartAfterBoot(context.Background()); err != nil {
			t.Fatal(err)
		}
		if m.state.RestartRequired == restarted {
			t.Fatalf("restarted=%t state=%+v", restarted, m.state)
		}
	}
}

func TestSetLinuxDockerPausedControlsOnlyOwnedDockerUnits(t *testing.T) {
	for _, test := range []struct {
		name   string
		paused bool
		action string
	}{
		{name: "pause", paused: true, action: "stop"},
		{name: "resume", paused: false, action: "start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := testManager(t)
			var got []string
			m.runCommand = func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
				got = append([]string(nil), args...)
				return nil, nil
			}
			if err := m.SetLinuxDockerPaused(context.Background(), test.paused); err != nil {
				t.Fatal(err)
			}
			units := []string{"docker.service", "docker.socket"}
			if !test.paused {
				units = []string{"docker.socket", "docker.service"}
			}
			want := append([]string{"--distribution", "UCD-Linux", "--user", "root", "--cd", "/", "--exec", "/usr/bin/systemctl", test.action}, units...)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("systemd target differs:\n got %#v\nwant %#v", got, want)
			}
		})
	}
}

func TestSetLinuxDockerPausedKeepsCommandError(t *testing.T) {
	m := testManager(t)
	want := errors.New("systemctl unavailable")
	m.runCommand = func(context.Context, io.Reader, ...string) ([]byte, error) {
		return nil, want
	}
	if err := m.SetLinuxDockerPaused(context.Background(), true); !errors.Is(err, want) {
		t.Fatalf("systemctl error was lost: %v", err)
	}
}

func TestConfigureRollsBackStateWhenConfigCannotBeWritten(t *testing.T) {
	m := testManager(t)
	m.settings.WSL.ProfileDirectory = filepath.Join(m.settings.DataRoot, "missing")
	if _, err := m.configure(8, 4096, 128); err == nil {
		t.Fatal("expected configuration error")
	}
	if m.state.CPUs != 4 {
		t.Fatalf("memory state changed: %+v", m.state)
	}
	m.state = wslState{}
	if err := m.loadState(); err != nil {
		t.Fatal(err)
	}
	if m.state.CPUs != 4 {
		t.Fatalf("persisted state not restored: %+v", m.state)
	}
}

func TestInvalidLimitsDoNotWriteConfiguration(t *testing.T) {
	m := testManager(t)
	original := m.state
	for _, tc := range []struct {
		name         string
		cpus         int
		memory, disk int64
	}{
		{"zero CPUs", 0, 4096, 128},
		{"too many CPUs", 129, 4096, 128},
		{"too little memory", 4, 511, 128},
		{"too much memory", 4, 1048577, 128},
		{"too little disk", 4, 4096, 15},
		{"too much disk", 4, 4096, 8193},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.configure(tc.cpus, tc.memory, tc.disk); err == nil {
				t.Fatal("accepted invalid limits")
			}
			if m.state != original {
				t.Fatalf("invalid change modified state: %+v", m.state)
			}
			for _, path := range []string{m.statePath(), filepath.Join(m.settings.WSL.ProfileDirectory, ".wslconfig")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("invalid change created %s or could not check it: %v", path, err)
				}
			}
		})
	}
	if err := validateLimits(1, 512, 16); err != nil {
		t.Fatalf("rejected lower limits: %v", err)
	}
	if err := validateLimits(128, 1048576, 8192); err != nil {
		t.Fatalf("rejected upper limits: %v", err)
	}
}

func TestPendingResizeWaitsForStoppedWSLAndExecutesOnce(t *testing.T) {
	m := testManager(t)
	m.state.DiskGB, m.state.DiskResizePending = 256, true
	var calls [][]string
	running := true
	m.runCommand = func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "--list" {
			if len(args) == 2 || running {
				return []byte("UCD-Linux\n"), nil
			}
		}
		return nil, nil
	}
	if err := m.applyPendingResize(context.Background()); err == nil {
		t.Fatal("resized while WSL was running")
	}
	if len(calls) != 1 || !m.state.DiskResizePending {
		t.Fatalf("unexpected calls or state: %v %+v", calls, m.state)
	}
	running = false
	if err := m.applyPendingResize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.state.DiskResizePending {
		t.Fatal("resize remains pending after success")
	}
	want := []string{"--manage", "UCD-Linux", "--resize", "256GB"}
	if !reflect.DeepEqual(calls[len(calls)-1], want) {
		t.Fatalf("got command %q, want %q", calls[len(calls)-1], want)
	}
}

func TestEmptyListingDoesNotAssumeWSLStopped(t *testing.T) {
	m := testManager(t)
	m.state.DiskResizePending = true
	m.runCommand = func(_ context.Context, _ io.Reader, _ ...string) ([]byte, error) { return nil, nil }
	if _, err := m.running(context.Background()); err == nil {
		t.Fatal("accepted unavailable WSL listings as stopped")
	}
	if err := m.applyPendingResize(context.Background()); err == nil {
		t.Fatal("accepted unavailable WSL listings for disk expansion")
	}
}

func TestSupervisorReportsFailureAndStopsOnCancellation(t *testing.T) {
	m := testManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := make(chan struct{})
	calls := 0
	m.runCommand = func(ctx context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls++
		want := []string{"--distribution", "UCD-Linux", "--user", "root", "--cd", "/", "--exec", "/usr/bin/sleep", "infinity"}
		if !reflect.DeepEqual(args, want) {
			return nil, errors.New("wrong keepalive command")
		}
		if calls == 1 {
			return nil, errors.New("distribution failed")
		}
		close(second)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not retry")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	events := m.events.Since(0)
	if len(events) != 1 || events[0].Type != "wsl.failed" || !strings.Contains(events[0].Message, "distribution failed") {
		t.Fatalf("unexpected events: %+v", events)
	}
}

func TestStatusClearsRecoveredSupervisorError(t *testing.T) {
	m := testManager(t)
	m.lastError = "previous keepalive failure"
	m.pendingError = "disk resize still pending"
	m.runCommand = func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] == "--list" {
			return []byte("UCD-Linux\n"), nil
		}
		return []byte("MemTotal: 4096000 kB\nMemAvailable: 2048000 kB\n"), nil
	}
	got, err := m.status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := got.(map[string]any)
	if result["running"] != true || result["error"] != "disk resize still pending" {
		t.Fatalf("unexpected status: %+v", result)
	}
	if m.lastError != "" {
		t.Fatalf("stale supervisor error remains: %q", m.lastError)
	}
}

func TestSharesUseArgumentVectorsAndPersist(t *testing.T) {
	m := testManager(t)
	path := filepath.Join(m.settings.DataRoot, "shares", "data $(literal) with spaces")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	m.runCommand = func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}
	created, err := m.createShare(context.Background(), "data", path)
	if err != nil {
		t.Fatal(err)
	}
	if created.(share).LinuxPath != "/mnt/ucd/data" {
		t.Fatalf("bad share: %+v", created)
	}
	prefix := []string{"--distribution", "UCD-Linux", "--user", "root", "--cd", "/", "--exec", "/bin/sh", "-c"}
	if len(calls) != 1 || len(calls[0]) < len(prefix)+3 || !reflect.DeepEqual(calls[0][:len(prefix)], prefix) || calls[0][len(prefix)] != addShareScript {
		t.Fatalf("unexpected mount call: %q", calls)
	}
	if strings.Contains(calls[0][len(prefix)], "$(literal)") {
		t.Fatal("share path interpolated into shell program")
	}
	if !strings.Contains(strings.Join(calls[0][len(prefix)+2:], "|"), "$(literal)") {
		t.Fatal("share path not passed as an argument")
	}
	if !strings.Contains(strings.Join(calls[0], "|"), `\040`) {
		t.Fatal("fstab path is not escaped")
	}
	shares, err := m.listShares()
	if err != nil || len(shares) != 1 {
		t.Fatalf("shares=%+v error=%v", shares, err)
	}
	if _, err := m.createShare(context.Background(), "data", path); err == nil {
		t.Fatal("accepted duplicate")
	}
	if _, err := m.createShare(context.Background(), "other", m.settings.DataRoot); err == nil {
		t.Fatal("accepted outside shared root")
	}
	if err := m.removeShare(context.Background(), "data"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || len(calls[1]) <= len(prefix) || !reflect.DeepEqual(calls[1][:len(prefix)], prefix) || calls[1][len(prefix)] != removeShareScript {
		t.Fatalf("unexpected unmount call: %q", calls)
	}
	shares, err = m.listShares()
	if err != nil || len(shares) != 0 {
		t.Fatalf("shares=%+v error=%v", shares, err)
	}
	if err := m.removeShare(context.Background(), "data"); err == nil {
		t.Fatal("removed missing share")
	}
}

func TestVerifyRootFSChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debian-rootfs.tar")
	data := []byte("rootfs data")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if err := os.WriteFile(path+".sha256", []byte(hex.EncodeToString(sum[:])+"  debian-rootfs.tar\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyRootFS(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyRootFS(path); err == nil {
		t.Fatal("accepted incorrect rootfs hash")
	}
}

func stageProcessToolsRootFS(t *testing.T, m *Manager, missing string) string {
	t.Helper()
	var data bytes.Buffer
	w := tar.NewWriter(&data)
	for _, name := range []string{"procps.deb", "libproc2-0.deb", "libncursesw6.deb", "libtinfo6.deb"} {
		if name == missing {
			continue
		}
		content := []byte("package " + name)
		if err := w.WriteHeader(&tar.Header{Name: "usr/local/share/ucd/procps/" + name, Mode: 0644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.settings.DataRoot, "bootstrap", "debian-rootfs.tar")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data.Bytes())
	if err := os.WriteFile(path+".sha256", []byte(hex.EncodeToString(sum[:])+"  debian-rootfs.tar\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnsureProcessToolsSkipsRootFSWhenPSExists(t *testing.T) {
	m := testManager(t)
	calls := 0
	m.runCommand = func(_ context.Context, input io.Reader, args ...string) ([]byte, error) {
		calls++
		if input != nil || args[len(args)-2] != "-c" || !strings.Contains(args[len(args)-1], "command -v ps") {
			t.Fatalf("unexpected process tools check: %q", args)
		}
		return []byte("present"), nil
	}
	if err := m.ensureProcessTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("got %d commands, want only the check", calls)
	}
}

func TestEnsureProcessToolsInstallsVerifiedPackagesOffline(t *testing.T) {
	m := testManager(t)
	stageProcessToolsRootFS(t, m, "")
	calls := 0
	m.runCommand = func(_ context.Context, input io.Reader, args ...string) ([]byte, error) {
		calls++
		switch calls {
		case 1:
			return []byte("missing"), nil
		case 2:
			if input == nil || !strings.Contains(strings.Join(args, " "), "/bin/tar -xf - -C /") {
				t.Fatalf("unexpected transfer: %q", args)
			}
			seen := make(map[string]string)
			r := tar.NewReader(input)
			for {
				h, err := r.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if h.Typeflag == tar.TypeDir {
					continue
				}
				content, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				seen[h.Name] = string(content)
			}
			for _, name := range []string{"procps.deb", "libproc2-0.deb", "libncursesw6.deb", "libtinfo6.deb"} {
				if seen["run/ucd-procps/"+name] != "package "+name {
					t.Fatalf("missing or altered offline package %s", name)
				}
			}
			if len(seen) != 4 {
				t.Fatalf("unexpected offline package files: %v", seen)
			}
		case 3:
			if input != nil || !strings.Contains(args[len(args)-1], `dpkg -i "$@"`) {
				t.Fatalf("install is not offline: %q", args)
			}
		default:
			t.Fatalf("unexpected command: %q", args)
		}
		return nil, nil
	}
	if err := m.ensureProcessTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("got %d commands, want check, transfer and install", calls)
	}
}

func TestEnsureProcessToolsRejectsUnverifiedOrIncompleteRootFS(t *testing.T) {
	for _, tc := range []struct {
		name, missing string
		tamper        bool
	}{
		{name: "checksum mismatch", tamper: true},
		{name: "missing package", missing: "libproc2-0.deb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(t)
			path := stageProcessToolsRootFS(t, m, tc.missing)
			if tc.tamper {
				if err := os.WriteFile(path+".sha256", []byte(strings.Repeat("0", 64)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			m.runCommand = func(_ context.Context, _ io.Reader, _ ...string) ([]byte, error) {
				calls++
				return []byte("missing"), nil
			}
			if err := m.ensureProcessTools(context.Background()); err == nil {
				t.Fatal("accepted unverified or incomplete rootfs")
			}
			if calls != 1 {
				t.Fatalf("unverified packages were transferred or installed (%d commands)", calls)
			}
		})
	}
}

func TestHelperArchiveIncludesPrivateFilesAndConfig(t *testing.T) {
	m := testManager(t)
	for _, name := range []string{"bootstrap/ucd-helper", "bootstrap/ucd-filetool", "bootstrap/docker-buildx", "bootstrap/ucd-helper.service", "certificates/ca.pem", "certificates/server.pem", "certificates/server-key.pem"} {
		path := filepath.Join(m.settings.DataRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := m.helperArchive()
	if err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(bytes.NewReader(archive))
	entries := map[string]*tar.Header{}
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[h.Name] = h
	}
	for _, name := range []string{"etc/ucd/server-key.pem.new", "etc/ucd/helper.json.new"} {
		if entries[name] == nil || entries[name].Mode != 0600 {
			t.Fatalf("missing or unsafe mode for %s: %+v", name, entries[name])
		}
	}
	if entries["usr/local/bin/ucd-helper.new"] == nil || entries["usr/local/bin/ucd-helper.new"].Mode != 0755 {
		t.Fatal("helper binary missing or not executable")
	}
	if entries["usr/local/bin/ucd-filetool.new"] == nil || entries["usr/local/bin/ucd-filetool.new"].Mode != 0755 {
		t.Fatal("container file utility missing or not executable")
	}
	if entries["usr/libexec/docker/cli-plugins/docker-buildx.new"] == nil || entries["usr/libexec/docker/cli-plugins/docker-buildx.new"].Mode != 0755 {
		t.Fatal("Buildx upgrade binary missing or not executable")
	}
}

func TestDecodeWSLOutputUTF16(t *testing.T) {
	if got := decodeWSLOutput([]byte{'U', 0, 'C', 0, 'D', 0, '\r', 0, '\n', 0}); got != "UCD\r\n" {
		t.Fatalf("got %q", got)
	}
}

func TestWSLCommandSeparatesDiagnostics(t *testing.T) {
	for _, mode := range []string{"utf8", "utf16"} {
		t.Run(mode, func(t *testing.T) {
			m := testManager(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestWSLCommandChild$")
			cmd.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot"), "UCD_WSL_OUTPUT_CHILD=" + mode}
			out, err := m.runWSLCommand(cmd)
			if err != nil || string(out) != "present" {
				t.Fatalf("stdout=%q error=%v; diagnostic output must not change a successful probe", out, err)
			}
			events := m.events.Since(0)
			if len(events) != 1 || events[0].Type != "wsl.diagnostic" || events[0].Message != "wsl: root session warning" {
				t.Fatalf("diagnostic was not retained separately: %+v", events)
			}
		})
	}
}

func TestWSLCommandPreservesFailure(t *testing.T) {
	m := testManager(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWSLCommandChild$")
	cmd.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot"), "UCD_WSL_OUTPUT_CHILD=failure"}
	out, err := m.runWSLCommand(cmd)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 || string(out) != "present" {
		t.Fatalf("stdout=%q error=%v; original output and exit failure must be retained", out, err)
	}
	if !strings.Contains(err.Error(), "wsl: root session warning") || !strings.Contains(err.Error(), "present") {
		t.Fatalf("failure lost diagnostics: %v", err)
	}
	if events := m.events.Since(0); len(events) != 0 {
		t.Fatalf("failed command diagnostics were also reported as successful: %+v", events)
	}
}

func TestWSLCommandPreservesCancellation(t *testing.T) {
	m := testManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWSLCommandChild$")
	if _, err := m.runWSLCommand(cmd); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestWSLCommandChild(t *testing.T) {
	mode := os.Getenv("UCD_WSL_OUTPUT_CHILD")
	if mode == "" {
		t.Skip("subprocess fixture")
	}
	_, _ = io.WriteString(os.Stdout, "present")
	warning := []byte("wsl: root session warning\n")
	if mode == "utf16" {
		var encoded []byte
		for _, b := range warning {
			encoded = append(encoded, b, 0)
		}
		warning = encoded
	}
	_, _ = os.Stderr.Write(warning)
	if mode == "failure" {
		os.Exit(17)
	}
	os.Exit(0)
}

func TestEngineActionsUseExpectedUnitsAndWindowsRouting(t *testing.T) {
	m := testManager(t)
	var calls [][]string
	m.runCommand = func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}
	for _, tc := range []struct {
		method string
		units  []string
	}{
		{"engine.start", []string{"start", "docker.service"}},
		{"engine.stop", []string{"stop", "docker.service", "docker.socket"}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			calls = nil
			if _, err := m.Call(context.Background(), api.Request{Method: tc.method, Engine: "linux"}); err != nil {
				t.Fatal(err)
			}
			want := append([]string{"--distribution", "UCD-Linux", "--user", "root", "--cd", "/", "--exec", "/bin/systemctl"}, tc.units...)
			if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
				t.Fatalf("got %q, want %q", calls, want)
			}
		})
	}
	// WindowsCall checks cancellation before contacting SCM, so this exercises
	// the actual routing without changing a service on the development host.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = nil
	for _, method := range []string{"engine.start", "engine.stop"} {
		_, err := m.Call(ctx, api.Request{Method: method, Engine: "windows"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: got %v, want cancellation from WindowsCall", method, err)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("Windows actions unexpectedly invoked WSL: %q", calls)
	}
}

func TestLinuxEngineStopReportsSystemctlFailure(t *testing.T) {
	m := testManager(t)
	failure := errors.New("systemctl failed")
	m.runCommand = func(context.Context, io.Reader, ...string) ([]byte, error) { return nil, failure }
	_, err := m.Call(context.Background(), api.Request{Method: "engine.stop", Engine: "linux"})
	if !errors.Is(err, failure) {
		t.Fatalf("got %v, want underlying failure", err)
	}
}
