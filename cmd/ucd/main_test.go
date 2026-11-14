package main

import "testing"

func TestIsDockerEntrypoint(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{`C:\Program Files\UnifiedContainerDesktop\docker.exe`, true},
		{`C:\Program Files\UnifiedContainerDesktop\DOCKER.EXE`, true},
		{`/opt/ucd/docker`, true},
		{`C:\Program Files\UnifiedContainerDesktop\ucd.exe`, false},
		{`C:\Program Files\Docker\Docker\resources\bin\docker.exe`, true},
	}
	for _, tt := range tests {
		if got := isDockerEntrypoint(tt.path); got != tt.want {
			t.Errorf("isDockerEntrypoint(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
