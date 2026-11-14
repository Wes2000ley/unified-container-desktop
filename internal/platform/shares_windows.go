//go:build windows

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/skyri/ucd/internal/api"
)

type share struct {
	Name        string `json:"name"`
	WindowsPath string `json:"windowsPath"`
	LinuxPath   string `json:"linuxPath"`
}

// Values are passed as positional arguments, never interpolated into these scripts.
const addShareScript = `set -eu
target=$1; source=$2; fstab_entry=$3
mkdir -p -- "$target"
if mountpoint -q -- "$target"; then echo 'share mount point is already in use' >&2; exit 1; fi
tmp=
cleanup() { if [ -n "$tmp" ]; then rm -f -- "$tmp"; fi; umount -- "$target" || true; }
trap cleanup EXIT
mount -t drvfs "$source" "$target"
tmp=$(mktemp /etc/.fstab-ucd.XXXXXX)
cat /etc/fstab > "$tmp"
printf '%s\n' "$fstab_entry" >> "$tmp"
chmod --reference=/etc/fstab "$tmp"
chown --reference=/etc/fstab "$tmp"
mv -f -- "$tmp" /etc/fstab
trap - EXIT`

const removeShareScript = `set -eu
target=$1; legacy_base=$2; marker=$3
if mountpoint -q -- "$target"; then umount -- "$target"; fi
# Older releases also mounted the whole Windows drive beneath this path.
if mountpoint -q -- "$legacy_base"; then umount -- "$legacy_base"; fi
tmp=$(mktemp /etc/.fstab-ucd.XXXXXX)
trap 'rm -f -- "$tmp"' EXIT
awk -v marker="$marker" 'index($0, marker) == 0 {print}' /etc/fstab > "$tmp"
chmod --reference=/etc/fstab "$tmp"
chown --reference=/etc/fstab "$tmp"
mv -f -- "$tmp" /etc/fstab`

func (m *Manager) sharePath() string { return filepath.Join(m.settings.DataRoot, "shares.json") }
func (m *Manager) readShares() ([]share, error) {
	b, err := os.ReadFile(m.sharePath())
	if os.IsNotExist(err) {
		return []share{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read shares: %w", err)
	}
	var shares []share
	if err := json.Unmarshal(b, &shares); err != nil {
		return nil, fmt.Errorf("decode shares: %w", err)
	}
	if shares == nil {
		shares = []share{}
	}
	return shares, nil
}
func (m *Manager) saveShares(shares []share) error {
	b, err := json.MarshalIndent(shares, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(m.sharePath(), b); err != nil {
		return fmt.Errorf("save shares: %w", err)
	}
	return nil
}
func (m *Manager) listShares() ([]share, error) {
	m.shareMu.Lock()
	defer m.shareMu.Unlock()
	return m.readShares()
}

func (m *Manager) createShare(ctx context.Context, name, source string) (any, error) {
	if !api.ValidName(name) {
		return nil, api.Fail("invalid_argument", "share name must start with a lowercase letter and contain only lowercase letters, digits, or hyphens")
	}
	if !filepath.IsAbs(source) || strings.IndexFunc(source, func(r rune) bool { return unicode.IsControl(r) || r == '#' }) >= 0 {
		return nil, api.Fail("invalid_argument", "share path must be an absolute local path without control characters or #")
	}
	path, err := resolveShareDirectory(source, m.settings.DataRoot)
	if err != nil {
		return nil, err
	}
	s := share{Name: name, WindowsPath: path, LinuxPath: "/mnt/ucd/" + name}
	m.shareMu.Lock()
	defer m.shareMu.Unlock()
	shares, err := m.readShares()
	if err != nil {
		return nil, err
	}
	for _, existing := range shares {
		if existing.Name == name {
			return nil, api.Fail("already_exists", "share name already exists")
		}
	}
	if err := m.mountShare(ctx, s); err != nil {
		return nil, err
	}
	if err := m.saveShares(append(shares, s)); err != nil {
		if undo := m.unmountShare(ctx, s); undo != nil {
			return nil, fmt.Errorf("%v; remove incomplete share: %w", err, undo)
		}
		return nil, err
	}
	return s, nil
}

func (m *Manager) mountShare(ctx context.Context, s share) error {
	marker := " # ucd-share:" + s.Name + ":"
	fstabEntry := fstabEscape(s.WindowsPath) + " " + fstabEscape(s.LinuxPath) + " drvfs defaults,nofail 0 0" + marker
	if _, err := m.linux(ctx, nil, "/bin/sh", "-c", addShareScript, "ucd-share", s.LinuxPath, s.WindowsPath, fstabEntry); err != nil {
		return fmt.Errorf("mount share %q: %w", s.Name, err)
	}
	return nil
}

func resolveShareDirectory(source, dataRoot string) (string, error) {
	path, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("resolve share path: %w", err)
	}
	dataRoot, err = filepath.EvalSymlinks(dataRoot)
	if err != nil {
		return "", fmt.Errorf("resolve UCD machine data directory: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect share path: %w", err)
	}
	if !info.IsDir() {
		return "", api.Fail("invalid_argument", "share path must be a directory")
	}
	drive := filepath.VolumeName(path)
	if len(drive) != 2 || drive[1] != ':' {
		return "", api.Fail("invalid_argument", "share path must be on a local drive")
	}
	path = filepath.Clean(path)
	if strings.EqualFold(path, drive+`\`) {
		return "", api.Fail("invalid_argument", "share path cannot be a drive root")
	}
	// The service data root contains machine credentials and infrastructure
	// state. Retain the existing administrator-managed shares subtree while
	// allowing approved directories elsewhere on local drives.
	insideDataRoot := strings.EqualFold(filepath.Clean(dataRoot), filepath.Clean(path)) || within(dataRoot, path)
	if insideDataRoot && !within(filepath.Join(dataRoot, "shares"), path) {
		return "", api.Fail("invalid_argument", "share path cannot expose UCD's machine data directory")
	}
	return path, nil
}

func fstabEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\134`)
	s = strings.ReplaceAll(s, " ", `\040`)
	return s
}
func (m *Manager) unmountShare(ctx context.Context, s share) error {
	_, err := m.linux(ctx, nil, "/bin/sh", "-c", removeShareScript, "ucd-share", s.LinuxPath, "/mnt/ucd-drives/"+s.Name, " # ucd-share:"+s.Name+":")
	if err != nil {
		return fmt.Errorf("unmount share %q: %w", s.Name, err)
	}
	return nil
}
func (m *Manager) removeShare(ctx context.Context, name string) error {
	if !api.ValidName(name) {
		return api.Fail("invalid_argument", "invalid share name")
	}
	m.shareMu.Lock()
	defer m.shareMu.Unlock()
	shares, err := m.readShares()
	if err != nil {
		return err
	}
	for i, s := range shares {
		if s.Name != name {
			continue
		}
		if err := m.unmountShare(ctx, s); err != nil {
			return err
		}
		remaining := append(shares[:i:i], shares[i+1:]...)
		if err := m.saveShares(remaining); err != nil {
			if undo := m.mountShare(ctx, s); undo != nil {
				return fmt.Errorf("%v; restore share: %w", err, undo)
			}
			return err
		}
		return nil
	}
	return api.Fail("not_found", "share not found")
}
