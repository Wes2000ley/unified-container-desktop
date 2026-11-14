package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/transferclient"
)

func (b *Bridge) download(ctx context.Context, engine, sessionID, name string, choose func(context.Context, string) (string, error)) (selected string, resultErr error) {
	if err := transferclient.Validate(engine, sessionID); err != nil {
		return "", err
	}
	defer func() {
		resultErr = errors.Join(resultErr, transferclient.Close(b.client, engine, sessionID))
	}()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	selected, err := choose(ctx, transferclient.Filename(name))
	if err != nil {
		return "", err
	}
	if selected == "" {
		return "", nil
	}
	if err := saveDownload(ctx, b.client, engine, sessionID, selected); err != nil {
		return "", err
	}
	return selected, nil
}

func saveDownload(ctx context.Context, caller api.Caller, engine, sessionID, destination string) (resultErr error) {
	if !filepath.IsAbs(destination) {
		return errors.New("download destination must be an absolute path")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".ucd-download-*")
	if err != nil {
		return fmt.Errorf("create temporary download: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, file.Close())
		}
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove temporary download: %w", err))
		}
	}()
	if _, err := transferclient.Copy(ctx, caller, engine, sessionID, file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("flush temporary download: %w", err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary download: %w", err)
	}
	closed = true
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), destination); err != nil {
		return fmt.Errorf("save download: %w", err)
	}
	return nil
}
