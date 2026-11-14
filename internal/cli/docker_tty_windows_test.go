//go:build windows

package cli

import (
	"context"
	"os"
	"os/signal"
	"testing"

	"golang.org/x/sys/windows"
)

func dockerTestConsole() bool {
	for _, file := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		var mode uint32
		if windows.GetConsoleMode(windows.Handle(file.Fd()), &mode) != nil {
			return false
		}
	}
	return true
}

func TestDockerConsoleInterrupt(t *testing.T) {
	if os.Getenv("UCD_TEST_REAL_INTERRUPT") != "1" {
		t.Skip("requires a real Windows console and a Ctrl+C event")
	}
	if !dockerTestConsole() {
		t.Fatal("test process does not have console handles")
	}
	install := fakeDockerInstall(t)
	config := t.TempDir()
	t.Setenv("UCD_TEST_DOCKER_CHILD", "1")
	t.Setenv("UCD_TEST_CONSOLE_CHILD", "1")
	t.Setenv("UCD_TEST_CONSOLE_SIGNAL", "1")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code := runDockerProgram(ctx, install, linuxDockerHost, []string{"--config", config, "run", "-it", "image"}, os.Stdin, os.Stdout, os.Stderr)
	if code != 0 {
		t.Fatalf("child did not receive Ctrl+C: exit %d", code)
	}
}

// Run separately inside a real console: the regular test runner may use pipes.
func TestDockerConsoleFileHandles(t *testing.T) {
	if os.Getenv("UCD_TEST_REAL_TERMINAL") != "1" {
		t.Skip("requires a real Windows console")
	}
	if !dockerTestConsole() {
		t.Fatal("test process does not have console handles")
	}
	install := fakeDockerInstall(t)
	config := t.TempDir()
	t.Setenv("UCD_TEST_DOCKER_CHILD", "1")
	t.Setenv("UCD_TEST_CONSOLE_CHILD", "1")
	code := runDockerProgram(context.Background(), install, linuxDockerHost, []string{"--config", config, "run", "-it", "image"}, os.Stdin, os.Stdout, os.Stderr)
	if code != 0 {
		t.Fatalf("child did not inherit console handles: exit %d", code)
	}
}
