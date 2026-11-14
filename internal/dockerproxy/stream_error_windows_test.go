//go:build windows

package dockerproxy

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestIsClosedStreamErrorWindowsPipeFailures(t *testing.T) {
	for _, err := range []error{
		windows.ERROR_BROKEN_PIPE,
		windows.ERROR_NO_DATA,
		windows.ERROR_PIPE_NOT_CONNECTED,
	} {
		if !isClosedStreamError(err) {
			t.Errorf("isClosedStreamError(%v) = false, want true", err)
		}
	}
	if isClosedStreamError(errors.New("unrelated stream error")) {
		t.Fatal("unrelated stream error was treated as a closed pipe")
	}
}
