//go:build !windows

package local

import (
	"context"
	"errors"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

func TestNonWindowsOperationsAreUnsupported(t *testing.T) {
	client := NewClient()
	if _, err := client.Call(context.Background(), api.Request{Method: "engine.list"}); !errors.Is(err, errUnsupported) {
		t.Fatalf("client error = %v", err)
	}
	if err := Serve(context.Background(), nil); !errors.Is(err, errUnsupported) {
		t.Fatalf("server error = %v", err)
	}
	if err := RunService(nil); !errors.Is(err, errUnsupported) {
		t.Fatalf("service error = %v", err)
	}
}
