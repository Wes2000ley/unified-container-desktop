package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

type bridgeCaller struct {
	request api.Request
	err     error
}

type testDesktopPreferencesStore struct {
	value DesktopPreferences
}

func (s *testDesktopPreferencesStore) Get() DesktopPreferences { return s.value }
func (s *testDesktopPreferencesStore) Set(value DesktopPreferences) error {
	s.value = value
	return nil
}

func (c *bridgeCaller) Call(ctx context.Context, request api.Request) (any, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing deadline")
	}
	c.request = request
	if c.err != nil {
		return nil, c.err
	}
	return json.RawMessage(`{"items":[1]}`), nil
}

func TestBridgeReturnsRawJSONAndForwardsRequest(t *testing.T) {
	client := &bridgeCaller{}
	bridge := Bridge{client: client}
	result, err := bridge.Call(context.Background(), "container.list", "linux", `{"example":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if result != `{"items":[1]}` {
		t.Fatalf("result: %q", result)
	}
	if client.request.Method != "container.list" || client.request.Engine != "linux" || string(client.request.Params) != `{"example":true}` {
		t.Fatalf("request: %#v", client.request)
	}
}

func TestBridgeReadsAndSavesDesktopPreferences(t *testing.T) {
	preferences := &testDesktopPreferencesStore{value: DesktopPreferences{
		StartAtLogin: true, StartMinimized: true, CloseToTray: true,
	}}
	bridge := Bridge{preferences: preferences}
	got, err := bridge.GetDesktopPreferences()
	if err != nil {
		t.Fatal(err)
	}
	if got != preferences.value {
		t.Fatalf("preferences: %#v", got)
	}
	want := DesktopPreferences{CloseToTray: false}
	if err := bridge.SetDesktopPreferences(want); err != nil {
		t.Fatal(err)
	}
	got, err = bridge.GetDesktopPreferences()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("saved preferences: %#v, want %#v", got, want)
	}
}

func TestBridgeRejectsInvalidRequestAndReportsServiceFailure(t *testing.T) {
	client := &bridgeCaller{}
	bridge := Bridge{client: client}
	if _, err := bridge.Call(context.Background(), "image.list", "invalid", "{}"); err == nil {
		t.Fatal("invalid engine accepted")
	}
	if _, err := bridge.Call(context.Background(), "image.list", "", "{"); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	client.err = errors.New("pipe unavailable")
	if _, err := bridge.Call(context.Background(), "image.list", "", "{}"); err == nil || !strings.Contains(err.Error(), "service is installed and running") {
		t.Fatalf("error: %v", err)
	}
	client.err = api.Fail("permission_denied", "administrator required")
	if _, err := bridge.Call(context.Background(), "image.list", "", "{}"); err == nil || err.Error() != "administrator required" {
		t.Fatalf("error: %v", err)
	}
}
