//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Download prompts for a destination and streams a transfer into that file.
// It returns an empty path when the user cancels the save dialog.
func (b *Bridge) Download(ctx context.Context, engine, sessionID, name string) (string, error) {
	return b.download(ctx, engine, sessionID, name, chooseDownload)
}

func chooseDownload(ctx context.Context, name string) (string, error) {
	app := application.Get()
	if app == nil {
		return "", errors.New("the desktop save dialog is unavailable")
	}
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		path, err := app.Dialog.SaveFile().SetFilename(name).PromptForSingleSelection()
		done <- result{path, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case selected := <-done:
		// Wails exposes its cancel sentinel from an internal package; the
		// pinned dialog implementation returns this exact message on cancel.
		if selected.err != nil && selected.err.Error() == "cancelled by user" {
			return "", nil
		}
		if selected.err != nil {
			return "", fmt.Errorf("open save dialog: %w", selected.err)
		}
		return selected.path, nil
	}
}
