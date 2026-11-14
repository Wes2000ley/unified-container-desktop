// Package transferclient streams transfer sessions from the local service.
package transferclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/skyri/ucd/internal/api"
)

const chunkSize = 256 << 10

// Validate checks the identifiers used by both download adapters.
func Validate(engine, sessionID string) error {
	if !api.ValidEngine(engine) {
		return errors.New("engine must be linux or windows")
	}
	if strings.TrimSpace(sessionID) == "" || len(sessionID) > 1024 {
		return errors.New("a valid transfer session ID is required")
	}
	return nil
}

// Copy reads one bounded chunk at a time without retaining previous chunks.
func Copy(ctx context.Context, caller api.Caller, engine, sessionID string, destination io.Writer) (int64, error) {
	if err := Validate(engine, sessionID); err != nil {
		return 0, err
	}
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		request, err := api.NewRequest("transfer.read", engine, api.TransferParams{SessionID: sessionID, MaxBytes: chunkSize})
		if err != nil {
			return written, fmt.Errorf("create transfer read request: %w", err)
		}
		result, err := caller.Call(ctx, request)
		if err != nil {
			return written, fmt.Errorf("read transfer: %w", err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return written, fmt.Errorf("encode transfer response: %w", err)
		}
		var chunk api.TransferReadResult
		if err := json.Unmarshal(encoded, &chunk); err != nil {
			return written, fmt.Errorf("decode transfer response: %w", err)
		}
		if len(chunk.Data) > chunkSize {
			return written, errors.New("transfer response exceeds the requested chunk size")
		}
		if len(chunk.Data) == 0 && !chunk.EOF {
			return written, errors.New("transfer returned an empty chunk without EOF")
		}
		if len(chunk.Data) != 0 {
			n, err := destination.Write(chunk.Data)
			written += int64(n)
			if err != nil {
				return written, fmt.Errorf("write transfer: %w", err)
			}
			if n != len(chunk.Data) {
				return written, io.ErrShortWrite
			}
		}
		if chunk.EOF {
			return written, nil
		}
	}
}

// Close releases a session even when its original request was canceled.
func Close(caller api.Caller, engine, sessionID string) error {
	if err := Validate(engine, sessionID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := api.NewRequest("transfer.close", engine, api.TransferParams{SessionID: sessionID})
	if err != nil {
		return fmt.Errorf("create transfer close request: %w", err)
	}
	if _, err := caller.Call(ctx, request); err != nil {
		return fmt.Errorf("close transfer: %w", err)
	}
	return nil
}

// Filename returns a safe suggested name for a browser or Windows save dialog.
func Filename(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	for len(name) > 240 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" {
		return "download"
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" ||
		(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9') {
		return "_" + name
	}
	return name
}
