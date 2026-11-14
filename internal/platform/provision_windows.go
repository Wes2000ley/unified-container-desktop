//go:build windows

package platform

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/skyri/ucd/internal/helper"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var getTickCount64 = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount64")

// EnsureProvisioned imports the staged Debian rootfs once, installs the helper
// and its TLS files, and waits for an authenticated helper response. It never
// unregisters a distribution or discards its data.
func (m *Manager) EnsureProvisioned(ctx context.Context) error {
	for _, dir := range []string{m.settings.DataRoot, m.settings.WSL.InstallDirectory, filepath.Join(m.settings.DataRoot, "shares")} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	archive, err := m.helperArchive()
	if err != nil {
		return err
	}
	registered, err := m.registeredDistro()
	if err != nil {
		return err
	}
	m.mu.Lock()
	state := m.state
	if err := writeAtomic(filepath.Join(m.settings.WSL.ProfileDirectory, ".wslconfig"), []byte(m.configText(state))); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("write service account WSL configuration: %w", err)
	}
	if err := m.saveState(state); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("persist WSL settings: %w", err)
	}
	m.mu.Unlock()
	if !registered {
		rootfs := filepath.Join(m.settings.DataRoot, "bootstrap", "debian-rootfs.tar")
		if err := verifyRootFS(rootfs); err != nil {
			return err
		}
		if _, err := m.runCommand(ctx, nil, "--import", m.settings.WSL.Distro, m.settings.WSL.InstallDirectory, rootfs, "--version", "2"); err != nil {
			return fmt.Errorf("import dedicated Debian distribution: %w", err)
		}
	} else if state.DiskResizePending {
		if err := m.applyPendingResize(ctx); err != nil {
			m.mu.Lock()
			m.pendingError = err.Error()
			m.mu.Unlock()
			m.publish("wsl.resize_pending", err.Error())
		}
	}
	if err := m.ensureProcessTools(ctx); err != nil {
		return err
	}
	if err := m.clearRestartAfterReboot(); err != nil {
		return err
	}
	if _, err := m.linux(ctx, bytes.NewReader(archive), "/bin/tar", "-xf", "-", "-C", "/"); err != nil {
		return fmt.Errorf("install Linux helper files: %w", err)
	}
	const installScript = `set -eu
mkdir -p /etc/docker
mv -f /usr/local/bin/ucd-helper.new /usr/local/bin/ucd-helper
mv -f /usr/local/bin/ucd-filetool.new /usr/local/bin/ucd-filetool
mkdir -p /usr/libexec/docker/cli-plugins
mv -f /usr/libexec/docker/cli-plugins/docker-buildx.new /usr/libexec/docker/cli-plugins/docker-buildx
mv -f /etc/systemd/system/ucd-helper.service.new /etc/systemd/system/ucd-helper.service
mv -f /etc/ucd/ca.pem.new /etc/ucd/ca.pem
mv -f /etc/ucd/server.pem.new /etc/ucd/server.pem
mv -f /etc/ucd/server-key.pem.new /etc/ucd/server-key.pem
mv -f /etc/ucd/helper.json.new /etc/ucd/helper.json
systemctl daemon-reload
systemctl enable ucd-helper.service
# A controller restart can leave Docker paused after the old in-memory gate is gone.
systemctl start docker.socket docker.service
systemctl restart ucd-helper.service`
	if _, err := m.linux(ctx, nil, "/bin/sh", "-c", installScript); err != nil {
		return fmt.Errorf("enable Linux helper: %w", err)
	}
	client, err := helper.NewClient(m.settings)
	if err != nil {
		return fmt.Errorf("prepare Linux helper readiness check: %w", err)
	}
	readyErr := client.WaitReady(ctx)
	client.Close()
	if readyErr != nil {
		return m.helperStartupError(ctx, readyErr)
	}
	m.publish("wsl.ready", "Dedicated Linux distribution and helper are ready")
	return nil
}

func (m *Manager) helperStartupError(ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("wait for Linux helper: %w", err)
	}
	journalCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := m.linux(journalCtx, nil, "/usr/bin/journalctl", "-u", "ucd-helper.service", "-n", "20", "--no-pager", "--output=cat")
	if err != nil {
		return fmt.Errorf("%w (read helper startup journal: %v)", cause, err)
	}
	journal := strings.TrimSpace(decodeWSLOutput(out))
	if len(journal) > 8192 {
		journal = strings.ToValidUTF8(journal[len(journal)-8192:], "")
	}
	if journal == "" {
		return cause
	}
	return fmt.Errorf("%w; helper startup journal:\n%s", cause, journal)
}

// ensureProcessTools upgrades existing distributions without replacing their data.
// The rootfs is only read if the daemon's process-listing utility is absent.
func (m *Manager) ensureProcessTools(ctx context.Context) error {
	const probe = `status=$(dpkg-query -W -f='${db:Status-Status}' procps 2>/dev/null || true)
if command -v ps >/dev/null 2>&1 && ps -ef >/dev/null 2>&1 && { [ -z "$status" ] || [ "$status" = installed ]; }; then printf present; else printf missing; fi`
	out, err := m.linux(ctx, nil, "/bin/sh", "-c", probe)
	if err != nil {
		return fmt.Errorf("check Linux process tools: %w", err)
	}
	switch strings.TrimSpace(decodeWSLOutput(out)) {
	case "present":
		return nil
	case "missing":
	default:
		return fmt.Errorf("check Linux process tools: unexpected response %q", strings.TrimSpace(decodeWSLOutput(out)))
	}
	rootfs := filepath.Join(m.settings.DataRoot, "bootstrap", "debian-rootfs.tar")
	archive, err := processToolsArchive(rootfs)
	if err != nil {
		return fmt.Errorf("prepare offline Linux process tools: %w", err)
	}
	if _, err := m.linux(ctx, bytes.NewReader(archive), "/bin/tar", "-xf", "-", "-C", "/"); err != nil {
		return fmt.Errorf("transfer offline Linux process tools: %w", err)
	}
	const install = `set -eu
trap 'rm -f /run/ucd-procps/procps.deb /run/ucd-procps/libproc2-0.deb /run/ucd-procps/libncursesw6.deb /run/ucd-procps/libtinfo6.deb; rmdir /run/ucd-procps 2>/dev/null || true' EXIT
export DEBIAN_FRONTEND=noninteractive
tinfo=$(dpkg-deb -f /run/ucd-procps/libtinfo6.deb Version)
if [ "$(dpkg-query -W -f='${db:Status-Status}' libncurses6 2>/dev/null || true)" = installed ]; then
    ncurses=$(dpkg-query -W -f='${Version}' libncurses6)
    if ! dpkg --compare-versions "$ncurses" eq "$tinfo"; then
        printf 'Cannot install offline libtinfo6 %s: installed libncurses6 %s requires a matching version; install matching versions of both libraries and retry.\n' "$tinfo" "$ncurses" >&2
        exit 1
    fi
fi
set --
for package in libtinfo6 libncursesw6 libproc2-0 procps; do
    file="/run/ucd-procps/$package.deb"
    available=$(dpkg-deb -f "$file" Version)
    status=$(dpkg-query -W -f='${db:Status-Status}' "$package" 2>/dev/null || true)
    if [ -n "$status" ] && [ "$status" != not-installed ]; then
        installed=$(dpkg-query -W -f='${Version}' "$package" 2>/dev/null || true)
        if [ -n "$installed" ] && dpkg --compare-versions "$installed" gt "$available"; then
            printf 'Cannot install offline %s %s: version %s is already installed; stage a rootfs with current packages.\n' "$package" "$available" "$installed" >&2
            exit 1
        fi
        if [ "$status" = installed ] && dpkg --compare-versions "$installed" eq "$available"; then continue; fi
    fi
    set -- "$@" "$file"
done
if [ "$#" -gt 0 ]; then dpkg -i "$@"; fi
ps -ef >/dev/null`
	if _, err := m.linux(ctx, nil, "/bin/sh", "-c", install); err != nil {
		return fmt.Errorf("install offline Linux process tools: %w", err)
	}
	return nil
}

// processToolsArchive extracts only the offline packages from a verified rootfs.
func processToolsArchive(rootfs string) ([]byte, error) {
	if err := verifyRootFS(rootfs); err != nil {
		return nil, err
	}
	f, err := os.Open(rootfs)
	if err != nil {
		return nil, fmt.Errorf("open staged Debian rootfs: %w", err)
	}
	defer f.Close()
	packages := map[string]bool{
		"procps.deb": false, "libproc2-0.deb": false,
		"libncursesw6.deb": false, "libtinfo6.deb": false,
	}
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	if err := w.WriteHeader(&tar.Header{Name: "run/ucd-procps/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		return nil, err
	}
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read staged Debian rootfs: %w", err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		const prefix = "usr/local/share/ucd/procps/"
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		name = strings.TrimPrefix(name, prefix)
		seen, wanted := packages[name]
		if !wanted {
			continue
		}
		if seen || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA) || h.Size <= 0 || h.Size > 8<<20 {
			return nil, fmt.Errorf("invalid offline package %s in staged Debian rootfs", name)
		}
		packages[name] = true
		if err := w.WriteHeader(&tar.Header{Name: "run/ucd-procps/" + name, Mode: 0644, Size: h.Size}); err != nil {
			return nil, err
		}
		if _, err := io.Copy(w, r); err != nil {
			return nil, fmt.Errorf("read offline package %s: %w", name, err)
		}
	}
	for name, found := range packages {
		if !found {
			return nil, fmt.Errorf("staged Debian rootfs is missing offline package %s", name)
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func verifyRootFS(path string) error {
	expected, err := os.ReadFile(path + ".sha256")
	if err != nil {
		return fmt.Errorf("read staged Debian checksum: %w", err)
	}
	fields := strings.Fields(string(expected))
	if len(fields) == 0 {
		return fmt.Errorf("staged Debian checksum is empty")
	}
	want, err := hex.DecodeString(fields[0])
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("staged Debian checksum is invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open staged Debian rootfs: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect staged Debian rootfs: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("staged Debian rootfs is not a regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash staged Debian rootfs: %w", err)
	}
	if !bytes.Equal(h.Sum(nil), want) {
		return fmt.Errorf("staged Debian rootfs SHA-256 does not match its checksum")
	}
	return nil
}

func (m *Manager) helperArchive() ([]byte, error) {
	type item struct {
		src, dst string
		mode     int64
	}
	items := []item{
		{filepath.Join(m.settings.DataRoot, "bootstrap", "ucd-helper"), "usr/local/bin/ucd-helper.new", 0755},
		{filepath.Join(m.settings.DataRoot, "bootstrap", "ucd-filetool"), "usr/local/bin/ucd-filetool.new", 0755},
		{filepath.Join(m.settings.DataRoot, "bootstrap", "docker-buildx"), "usr/libexec/docker/cli-plugins/docker-buildx.new", 0755},
		{filepath.Join(m.settings.DataRoot, "bootstrap", "ucd-helper.service"), "etc/systemd/system/ucd-helper.service.new", 0644},
		{filepath.Join(m.settings.DataRoot, "certificates", "ca.pem"), "etc/ucd/ca.pem.new", 0644},
		{filepath.Join(m.settings.DataRoot, "certificates", "server.pem"), "etc/ucd/server.pem.new", 0644},
		{filepath.Join(m.settings.DataRoot, "certificates", "server-key.pem"), "etc/ucd/server-key.pem.new", 0600},
	}
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	if err := w.WriteHeader(&tar.Header{Name: "etc/ucd/", Typeflag: tar.TypeDir, Mode: 0700}); err != nil {
		return nil, err
	}
	for _, entry := range items {
		data, err := os.ReadFile(entry.src)
		if err != nil {
			return nil, fmt.Errorf("read staged helper file %s: %w", entry.src, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("staged helper file %s is empty", entry.src)
		}
		if err := w.WriteHeader(&tar.Header{Name: entry.dst, Mode: entry.mode, Size: int64(len(data))}); err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	config := []byte("{}\n")
	if err := w.WriteHeader(&tar.Header{Name: "etc/ucd/helper.json.new", Mode: 0600, Size: int64(len(config))}); err != nil {
		return nil, err
	}
	if _, err := w.Write(config); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (m *Manager) registeredDistro() (bool, error) {
	path := m.profile.sid + `\Software\Microsoft\Windows\CurrentVersion\Lxss`
	key, err := registry.OpenKey(registry.USERS, path, registry.ENUMERATE_SUB_KEYS)
	if err == syscall.ERROR_FILE_NOT_FOUND {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect service account distributions: %w", err)
	}
	defer key.Close()
	names, err := key.ReadSubKeyNames(-1)
	if err != nil {
		return false, fmt.Errorf("enumerate service account distributions: %w", err)
	}
	for _, name := range names {
		distro, err := registry.OpenKey(key, name, registry.QUERY_VALUE)
		if err != nil {
			return false, fmt.Errorf("inspect distribution registration: %w", err)
		}
		distroName, _, nameErr := distro.GetStringValue("DistributionName")
		if nameErr != nil {
			_ = distro.Close()
			return false, fmt.Errorf("read distribution name: %w", nameErr)
		}
		if !strings.EqualFold(distroName, m.settings.WSL.Distro) {
			_ = distro.Close()
			continue
		}
		base, _, baseErr := distro.GetStringValue("BasePath")
		version, _, versionErr := distro.GetIntegerValue("Version")
		_ = distro.Close()
		if baseErr != nil {
			return false, fmt.Errorf("read distribution location: %w", baseErr)
		}
		if versionErr != nil {
			return false, fmt.Errorf("read distribution version: %w", versionErr)
		}
		base = strings.TrimPrefix(base, `\\?\`)
		if version != 2 || !strings.EqualFold(filepath.Clean(base), filepath.Clean(m.settings.WSL.InstallDirectory)) {
			return false, fmt.Errorf("existing %s distribution must be WSL 2 and located at %s", m.settings.WSL.Distro, m.settings.WSL.InstallDirectory)
		}
		return true, nil
	}
	return false, nil
}

func (m *Manager) applyPendingResize(ctx context.Context) error {
	m.mu.Lock()
	disk := m.state.DiskGB
	m.mu.Unlock()
	out, err := m.runCommand(ctx, nil, "--list", "--running", "--quiet")
	if err != nil {
		return fmt.Errorf("check WSL before disk resize: %w", err)
	}
	if strings.TrimSpace(decodeWSLOutput(out)) != "" {
		return fmt.Errorf("WSL disk expansion is pending: stop the service-owned WSL VM and restart the controller, or restart Windows")
	}
	if err := m.verifyListOutput(ctx); err != nil {
		return err
	}
	if _, err := m.runCommand(ctx, nil, "--manage", m.settings.WSL.Distro, "--resize", fmt.Sprintf("%dGB", disk)); err != nil {
		return fmt.Errorf("expand WSL virtual disk: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.state
	if next.DiskGB == disk {
		next.DiskResizePending = false
	}
	if err := m.saveState(next); err != nil {
		return fmt.Errorf("save resized disk state: %w", err)
	}
	m.state = next
	return nil
}

func (m *Manager) clearRestartAfterReboot() error {
	ticks, _, _ := getTickCount64.Call()
	boot := time.Now().Add(-time.Duration(ticks) * time.Millisecond)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.RestartRequired || m.state.DiskResizePending || m.state.ChangedAt.IsZero() || !boot.After(m.state.ChangedAt) {
		return nil
	}
	next := m.state
	next.RestartRequired = false
	if err := m.saveState(next); err != nil {
		return fmt.Errorf("save WSL restart state: %w", err)
	}
	m.state = next
	return nil
}
