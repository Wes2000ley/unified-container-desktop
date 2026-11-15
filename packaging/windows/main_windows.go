// The Burn bundle caches this launcher and its verified payloads together.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func main() {
	mode := flag.String("mode", "Check", "Check, Features, or Wsl")
	logPath := flag.String("log", "", "prerequisite log path")
	flag.Parse()
	if (*mode != "Check" && *mode != "Features" && *mode != "Wsl") || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "Invalid prerequisite mode; use Check, Features, or Wsl.")
		os.Exit(1603)
	}
	if err := run(*mode, *logPath); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode()) // Preserve 3010 so Burn resumes at the next package.
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1603)
	}
}

func run(mode, logPath string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(filepath.Dir(executable), "Setup.ps1"), "-Mode", mode)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if logPath != "" {
		log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return fmt.Errorf("open prerequisite log: %w", err)
		}
		defer log.Close()
		cmd.Stdout, cmd.Stderr = log, log
		fmt.Fprintln(log, "Unified Container Desktop prerequisites:", mode)
	}
	return cmd.Run()
}
