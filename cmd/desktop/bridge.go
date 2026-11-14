package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/skyri/ucd/internal/api"
)

// Bridge exposes controller operations to the native frontend.
type Bridge struct {
	client      api.Caller
	preferences desktopPreferencesStore
}

// Call sends JSON parameters to the local machine service and returns its JSON result.
// Wails supplies ctx and cancels it if the frontend call is cancelled.
func (b *Bridge) Call(ctx context.Context, method, engine, paramsJSON string) (string, error) {
	if method == "" {
		return "", errors.New("method is required")
	}
	if engine != "" && !api.ValidEngine(engine) {
		return "", errors.New("engine must be linux or windows")
	}
	if paramsJSON == "" {
		paramsJSON = "{}"
	}
	if !json.Valid([]byte(paramsJSON)) {
		return "", errors.New("parameters must be valid JSON")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	result, err := b.client.Call(ctx, api.Request{Method: method, Engine: engine, Params: json.RawMessage(paramsJSON)})
	if err != nil {
		var serviceError *api.Error
		if errors.As(err, &serviceError) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", fmt.Errorf("call the Unified Container Desktop service over its local named pipe: %w; verify the service is installed and running", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode service response: %w", err)
	}
	return string(encoded), nil
}

// GetDesktopPreferences returns desktop settings for the current Windows user.
func (b *Bridge) GetDesktopPreferences() (DesktopPreferences, error) {
	if b.preferences == nil {
		return DesktopPreferences{}, errors.New("desktop preferences are unavailable")
	}
	return b.preferences.Get(), nil
}

// SetDesktopPreferences updates desktop settings for the current Windows user.
func (b *Bridge) SetDesktopPreferences(preferences DesktopPreferences) error {
	if b.preferences == nil {
		return errors.New("desktop preferences are unavailable")
	}
	return b.preferences.Set(preferences)
}
