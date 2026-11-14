//go:build windows

package main

import "testing"

func TestStartupCommandQuotesExecutablePath(t *testing.T) {
	got := startupCommand(`C:\Program Files\Unified Container Desktop\desktop.exe`)
	want := `"C:\Program Files\Unified Container Desktop\desktop.exe" --background`
	if got != want {
		t.Fatalf("startup command: %q, want %q", got, want)
	}
}
