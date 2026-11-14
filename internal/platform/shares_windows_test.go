//go:build windows

package platform

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateShareAllowsApprovedDirectoryOutsideProgramData(t *testing.T) {
	m := testManager(t)
	path := filepath.Join(t.TempDir(), "project files")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	m.runCommand = func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}

	created, err := m.createShare(context.Background(), "project-data", path)
	if err != nil {
		t.Fatal(err)
	}
	result := created.(share)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.WindowsPath != resolved || result.LinuxPath != "/mnt/ucd/project-data" {
		t.Fatalf("unexpected share mapping: %+v", result)
	}
	if len(calls) != 1 {
		t.Fatalf("expected one mount operation, got %q", calls)
	}
	prefix := []string{"--distribution", "UCD-Linux", "--user", "root", "--cd", "/", "--exec", "/bin/sh", "-c", addShareScript, "ucd-share"}
	if len(calls[0]) != len(prefix)+3 || !strings.EqualFold(strings.Join(calls[0][:len(prefix)], "|"), strings.Join(prefix, "|")) {
		t.Fatalf("unexpected direct mount arguments: %q", calls[0])
	}
	if calls[0][len(prefix)] != result.LinuxPath || calls[0][len(prefix)+1] != result.WindowsPath {
		t.Fatalf("new share did not mount the approved directory directly: %q", calls[0])
	}
	call := strings.Join(calls[0], "|")
	if !strings.Contains(call, "project files") || !strings.Contains(call, `project\040files`) {
		t.Fatalf("mount source does not include the selected directory: %q", calls[0])
	}
	if strings.Contains(addShareScript, "mount --bind") || strings.Contains(addShareScript, "/mnt/ucd-drives") {
		t.Fatal("new share exposes its Windows drive root")
	}
	shares, err := m.listShares()
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 || shares[0] != result {
		t.Fatalf("persisted shares=%+v, created=%+v", shares, result)
	}
}

func TestResolveShareDirectoryRejectsDriveAndMachineDataRoots(t *testing.T) {
	m := testManager(t)
	volume := filepath.VolumeName(m.settings.DataRoot)
	if path, err := resolveShareDirectory(volume+`\`, m.settings.DataRoot); err == nil {
		t.Fatalf("accepted drive root %q", path)
	}
	if path, err := resolveShareDirectory(m.settings.DataRoot, m.settings.DataRoot); err == nil {
		t.Fatalf("accepted UCD machine-data root %q", path)
	}
}

func TestResolveShareDirectoryAllowsDedicatedSharesSubdirectory(t *testing.T) {
	m := testManager(t)
	path := filepath.Join(m.settings.DataRoot, "shares", "project")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveShareDirectory(path, m.settings.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != path {
		t.Fatalf("resolved %q, want %q", resolved, path)
	}
}

func TestSharePathValidationRejectsUNCAndUnsafeText(t *testing.T) {
	m := testManager(t)
	m.runCommand = func(context.Context, io.Reader, ...string) ([]byte, error) {
		return nil, nil
	}
	for _, path := range []string{`\\server\share`, "relative", filepath.Join(m.settings.DataRoot, "shares", "line\nbreak")} {
		if _, err := m.createShare(context.Background(), "unsafe", path); err == nil {
			t.Errorf("accepted unsafe path %q", path)
		}
	}
	if _, err := m.createShare(context.Background(), "unsafe", m.settings.DataRoot); err == nil {
		t.Fatal("accepted UCD machine-data root")
	}
}

func TestShareRemovalCleansOldDriveMountAndAllMarkedFstabRows(t *testing.T) {
	m := testManager(t)
	var calls [][]string
	m.runCommand = func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, nil
	}
	legacy := share{Name: "old-share", WindowsPath: `C:\source`, LinuxPath: "/mnt/ucd/old-share"}
	if err := m.unmountShare(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected one removal operation, got %q", calls)
	}
	arguments := calls[0]
	if arguments[len(arguments)-3] != legacy.LinuxPath || arguments[len(arguments)-2] != "/mnt/ucd-drives/old-share" {
		t.Fatalf("legacy removal does not unmount both paths: %q", arguments)
	}
	if arguments[len(arguments)-1] != " # ucd-share:old-share:" {
		t.Fatalf("legacy fstab rows will not match the share marker: %q", arguments)
	}
	for _, expected := range []string{"legacy_base", "index($0, marker) == 0"} {
		if !strings.Contains(removeShareScript, expected) {
			t.Errorf("legacy cleanup script does not contain %q", expected)
		}
	}
}
