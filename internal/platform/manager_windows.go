//go:build windows

// Package platform manages the service-owned WSL distribution and Windows infrastructure.
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/skyri/ucd/internal/api"
	"golang.org/x/sys/windows"
)

type commandRunner func(context.Context, io.Reader, ...string) ([]byte, error)

type wslState struct {
	CPUs              int       `json:"cpus"`
	MemoryMB          int64     `json:"memoryMB"`
	DiskGB            int64     `json:"diskGB"`
	DiskResizePending bool      `json:"diskResizePending"`
	RestartRequired   bool      `json:"restartRequired"`
	ChangedAt         time.Time `json:"changedAt,omitempty"`
}

// Manager owns the loaded service profile and supervises its dedicated WSL distribution.
type Manager struct {
	settings     api.Settings
	events       *api.EventBus
	profile      *serviceProfile
	runCommand   commandRunner
	mu           sync.Mutex
	shareMu      sync.Mutex
	state        wslState
	lastError    string
	pendingError string
	closed       bool
}

// New validates the dedicated account and loads its profile. Close releases the profile.
func New(settings api.Settings, events *api.EventBus) (*Manager, error) {
	if settings.WSL.Distro != "UCD-Linux" || !strings.EqualFold(strings.TrimPrefix(settings.WSL.ServiceAccount, `.\`), "UcdService") {
		return nil, fmt.Errorf("WSL must use the dedicated UCD-Linux distribution and .\\UcdService account")
	}
	if windows.RtlGetVersion().BuildNumber < 22621 {
		return nil, fmt.Errorf("mirrored networking requires Windows 11 build 22621 or later")
	}
	if !filepath.IsAbs(settings.DataRoot) || !within(settings.DataRoot, settings.WSL.ProfileDirectory) || !within(settings.DataRoot, settings.WSL.InstallDirectory) {
		return nil, fmt.Errorf("WSL profile and install directories must be inside the absolute data root")
	}
	profile, err := openServiceProfile(settings.WSL.ServiceAccount, settings.WSL.ProfileDirectory)
	if err != nil {
		return nil, err
	}
	m := &Manager{settings: settings, events: events, profile: profile}
	m.runCommand = m.runWSL
	m.state = wslState{CPUs: settings.WSL.CPUs, MemoryMB: settings.WSL.MemoryMB, DiskGB: settings.WSL.DiskGB}
	if err := m.loadState(); err != nil {
		_ = profile.close()
		return nil, err
	}
	if err := validateLimits(m.state.CPUs, m.state.MemoryMB, m.state.DiskGB); err != nil {
		_ = profile.close()
		return nil, err
	}
	return m, nil
}

func within(root, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, `..\`)
}

func (m *Manager) runWSL(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, filepath.Join(system, "wsl.exe"), args...)
	cmd.Env = m.profile.env
	cmd.Stdin = input
	return m.runWSLCommand(cmd)
}

func (m *Manager) runWSLCommand(cmd *exec.Cmd) ([]byte, error) {
	args := cmd.Args[1:]
	// WSL can emit diagnostics even when the Linux command succeeds. Keep
	// them out of stdout, which callers parse as command results.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	diagnostic := strings.TrimSpace(decodeWSLOutput(stderr.Bytes()))
	if err != nil {
		detail := strings.TrimSpace(decodeWSLOutput(out))
		if diagnostic != "" {
			detail = strings.TrimSpace(detail + "\n" + diagnostic)
		}
		if detail != "" {
			return out, fmt.Errorf("wsl %s: %s: %w", strings.Join(args[:min(len(args), 3)], " "), detail, err)
		}
		return out, fmt.Errorf("wsl %s: %w", strings.Join(args[:min(len(args), 3)], " "), err)
	}
	if diagnostic != "" {
		m.publish("wsl.diagnostic", diagnostic)
	}
	return out, nil
}

func decodeWSLOutput(b []byte) string {
	if len(b) >= 2 && (b[0] == 0xff && b[1] == 0xfe || b[1] == 0) {
		if b[0] == 0xff && b[1] == 0xfe {
			b = b[2:]
		}
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[i*2]) | uint16(b[i*2+1])<<8
		}
		return strings.TrimRight(string(utf16.Decode(u)), "\x00")
	}
	return strings.ReplaceAll(string(b), "\x00", "")
}

func (m *Manager) linux(ctx context.Context, input io.Reader, args ...string) ([]byte, error) {
	// An inherited Windows working directory can resolve inside a controlled
	// share's drive mount, keeping that mount busy during removal.
	prefix := []string{"--distribution", m.settings.WSL.Distro, "--user", "root", "--cd", "/", "--exec"}
	return m.runCommand(ctx, input, append(prefix, args...)...)
}

// SetLinuxDockerPaused stops or starts the Docker daemon in the owned WSL
// distribution. The WSL distribution, controller helper, and other WSL
// distributions are left running.
func (m *Manager) SetLinuxDockerPaused(ctx context.Context, paused bool) error {
	action := "start"
	if paused {
		action = "stop"
	}
	units := []string{"docker.service", "docker.socket"}
	if !paused {
		units = []string{"docker.socket", "docker.service"}
	}
	_, err := m.linux(ctx, nil, append([]string{"/usr/bin/systemctl", action}, units...)...)
	if err != nil {
		return fmt.Errorf("%s dedicated Linux Docker service and socket: %w", action, err)
	}
	return nil
}

// Run keeps WSL alive and retries unexpected exits until ctx is canceled.
// Canceling only stops the owned keepalive process; it never terminates Docker or WSL.
func (m *Manager) Run(ctx context.Context) error {
	delay := time.Second
	for {
		started := time.Now()
		_, err := m.linux(ctx, nil, "/usr/bin/sleep", "infinity")
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("WSL keepalive exited unexpectedly")
		}
		m.setError(err)
		m.publish("wsl.failed", err.Error())
		if time.Since(started) >= time.Minute {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay = min(delay*2, 30*time.Second)
		}
	}
}

func (m *Manager) setError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.lastError = ""
	} else {
		m.lastError = err.Error()
	}
}
func (m *Manager) publish(kind, message string) {
	if m.events != nil {
		m.events.Publish(api.Event{Engine: "linux", Type: kind, Message: message})
	}
}

// Close releases the loaded profile after Run and requests have stopped.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	return m.profile.close()
}

// WSLSettings returns the currently requested limits along with fixed distribution details.
func (m *Manager) WSLSettings() api.WSLSettings {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.settings.WSL
	s.CPUs, s.MemoryMB, s.DiskGB = m.state.CPUs, m.state.MemoryMB, m.state.DiskGB
	return s
}

// Call handles infrastructure operations. Authentication is performed by the controller.
func (m *Manager) Call(ctx context.Context, req api.Request) (any, error) {
	switch req.Method {
	case "wsl.status":
		return m.status(ctx)
	case "wsl.configure":
		p, err := api.Decode[struct {
			CPUs     int   `json:"cpus"`
			MemoryMB int64 `json:"memoryMB"`
			DiskGB   int64 `json:"diskGB"`
		}](req)
		if err != nil {
			return nil, err
		}
		return m.configure(p.CPUs, p.MemoryMB, p.DiskGB)
	case "engine.start", "engine.stop":
		if !api.ValidEngine(req.Engine) {
			return nil, api.Fail("invalid_argument", "engine must be windows or linux")
		}
		action := strings.TrimPrefix(req.Method, "engine.")
		if req.Engine == "windows" {
			return WindowsCall(ctx, req)
		}
		units := []string{"/bin/systemctl", action, "docker.service"}
		if action == "stop" {
			units = append(units, "docker.socket")
		}
		_, err := m.linux(ctx, nil, units...)
		return nil, err
	case "operator.list":
		return WindowsCall(ctx, req)
	case "operator.add", "operator.remove":
		return WindowsCall(ctx, req)
	case "share.list":
		return m.listShares()
	case "share.create":
		p, err := api.Decode[struct {
			Name        string `json:"name"`
			WindowsPath string `json:"windowsPath"`
		}](req)
		if err != nil {
			return nil, err
		}
		return m.createShare(ctx, p.Name, p.WindowsPath)
	case "share.remove":
		p, err := api.Decode[struct {
			Name string `json:"name"`
		}](req)
		if err != nil {
			return nil, err
		}
		return nil, m.removeShare(ctx, p.Name)
	default:
		return nil, api.Fail("not_found", "unknown infrastructure method")
	}
}

func validateLimits(cpus int, memory, disk int64) error {
	if cpus < 1 || cpus > 128 || memory < 512 || memory > 1048576 || disk < 16 || disk > 8192 {
		return api.Fail("invalid_argument", "WSL limits must be CPUs 1–128, memory 512–1048576 MiB, and disk 16–8192 GiB")
	}
	return nil
}

func (m *Manager) statePath() string { return filepath.Join(m.settings.DataRoot, "wsl-settings.json") }
func (m *Manager) loadState() error {
	b, err := os.ReadFile(m.statePath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read WSL settings: %w", err)
	}
	if err := json.Unmarshal(b, &m.state); err != nil {
		return fmt.Errorf("decode WSL settings: %w", err)
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".ucd-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (m *Manager) configText(state wslState) string {
	swap := strings.ReplaceAll(filepath.Join(m.settings.DataRoot, "wsl", "swap.vhdx"), `\`, `\\`)
	return fmt.Sprintf("[wsl2]\nprocessors=%d\nmemory=%dMB\ndefaultVhdSize=%dGB\nswapFile=%s\nnetworkingMode=mirrored\ndnsTunneling=true\ntelemetry=false\n[experimental]\nautoMemoryReclaim=gradual\nhostAddressLoopback=true\n[general]\ninstanceIdleTimeout=-1\n", state.CPUs, state.MemoryMB, state.DiskGB, swap)
}

func (m *Manager) saveState(state wslState) error {
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(m.statePath(), b)
}

func (m *Manager) configure(cpus int, memory, disk int64) (any, error) {
	if err := validateLimits(cpus, memory, disk); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if disk < m.state.DiskGB {
		return nil, api.Fail("invalid_argument", "shrinking the WSL virtual disk is not supported")
	}
	if cpus == m.state.CPUs && memory == m.state.MemoryMB && disk == m.state.DiskGB {
		return m.state, nil
	}
	next := m.state
	next.CPUs, next.MemoryMB, next.DiskGB = cpus, memory, disk
	next.DiskResizePending = next.DiskResizePending || disk > m.state.DiskGB
	next.RestartRequired = true
	next.ChangedAt = time.Now().UTC()
	if err := m.saveState(next); err != nil {
		return nil, fmt.Errorf("save WSL settings: %w", err)
	}
	if err := writeAtomic(filepath.Join(m.settings.WSL.ProfileDirectory, ".wslconfig"), []byte(m.configText(next))); err != nil {
		if rollbackErr := m.saveState(m.state); rollbackErr != nil {
			return nil, fmt.Errorf("write WSL config: %v; restore settings: %w", err, rollbackErr)
		}
		return nil, fmt.Errorf("write WSL config: %w", err)
	}
	m.state = next
	return next, nil
}

func (m *Manager) running(ctx context.Context) (bool, error) {
	out, err := m.runCommand(ctx, nil, "--list", "--running", "--quiet")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(decodeWSLOutput(out)) == "" {
		if err := m.verifyListOutput(ctx); err != nil {
			return false, err
		}
	}
	return listContains(out, m.settings.WSL.Distro), nil
}

func listContains(out []byte, name string) bool {
	for _, line := range strings.Split(decodeWSLOutput(out), "\n") {
		if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*")), name) {
			return true
		}
	}
	return false
}

func (m *Manager) verifyListOutput(ctx context.Context) error {
	out, err := m.runCommand(ctx, nil, "--list", "--quiet")
	if err != nil {
		return fmt.Errorf("verify WSL distribution listing: %w", err)
	}
	if !listContains(out, m.settings.WSL.Distro) {
		return fmt.Errorf("WSL did not list the registered %s distribution; cannot determine its running state", m.settings.WSL.Distro)
	}
	return nil
}

func (m *Manager) status(ctx context.Context) (any, error) {
	m.mu.Lock()
	state, last, pending := m.state, m.lastError, m.pendingError
	m.mu.Unlock()
	result := map[string]any{"distro": m.settings.WSL.Distro, "cpus": state.CPUs, "memoryMB": state.MemoryMB, "diskGB": state.DiskGB, "diskResizePending": state.DiskResizePending, "restartRequired": state.RestartRequired}
	running, err := m.running(ctx)
	result["running"] = running
	if err != nil {
		result["error"] = err.Error()
	} else if running {
		m.setError(nil)
	} else if last != "" {
		result["error"] = last
	}
	if pending != "" {
		result["error"] = pending
	}
	if allocated, err := allocatedFileBytes(filepath.Join(m.settings.WSL.InstallDirectory, "ext4.vhdx")); err == nil {
		result["diskUsedBytes"] = allocated
	}
	if running {
		if out, err := m.linux(ctx, nil, "/bin/cat", "/proc/meminfo"); err == nil {
			var total, available int64
			for _, line := range strings.Split(string(out), "\n") {
				f := strings.Fields(line)
				if len(f) < 2 {
					continue
				}
				n, _ := strconv.ParseInt(f[1], 10, 64)
				if f[0] == "MemTotal:" {
					total = n
				}
				if f[0] == "MemAvailable:" {
					available = n
				}
			}
			if total > 0 && available > 0 {
				result["memoryUsedMB"] = (total - available) / 1024
			}
		}
	}
	return result, nil
}
