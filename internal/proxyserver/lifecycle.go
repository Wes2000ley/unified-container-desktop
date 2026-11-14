package proxyserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/skyri/ucd/internal/api"
)

// Run owns the loopback proxy listener until ctx is canceled. It reconciles
// proxy settings immediately and once per second, reports each distinct error
// until recovery, retries failed binds and updates, and closes its listener on
// shutdown. Clearing SOCKS5 disables the existing listener while a daemon
// restart is pending.
func Run(ctx context.Context, address string, source func() api.ProxySettings, report func(error)) error {
	if ctx == nil {
		return errors.New("proxy lifecycle context is required")
	}
	if source == nil {
		return errors.New("proxy settings source is required")
	}
	if err := ctx.Err(); err != nil {
		return nil
	}

	var server *Server
	var active api.ProxySettings
	var lastError string
	reportError := func(err error) {
		if err == nil {
			recovered := lastError != ""
			lastError = ""
			if recovered && report != nil {
				report(nil)
			}
			return
		}
		message := err.Error()
		if message == lastError {
			return
		}
		lastError = message
		if report != nil {
			report(err)
		}
	}
	closeServer := func() error {
		if server == nil {
			return nil
		}
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Close(closeCtx)
		server = nil
		return err
	}

	reconcile := func() {
		if server != nil {
			select {
			case <-server.Done():
				err := server.Err()
				if err == nil {
					err = errors.New("SOCKS5 HTTP bridge stopped unexpectedly")
				}
				reportError(err)
				_ = closeServer()
				active = api.ProxySettings{}
			default:
			}
		}

		settings := source()
		if settings.SOCKS5 == "" {
			if server == nil || settings == active {
				return
			}
			if err := server.Update(settings); err != nil {
				reportError(fmt.Errorf("disable SOCKS5 HTTP bridge: %w", err))
				return
			}
			active = settings
			reportError(nil)
			return
		}
		if server == nil {
			running, err := Start(ctx, address, settings)
			if err != nil {
				reportError(err)
				return
			}
			server, active = running, settings
			reportError(nil)
			return
		}
		if settings == active {
			return
		}
		if err := server.Update(settings); err != nil {
			reportError(fmt.Errorf("update SOCKS5 HTTP bridge: %w", err))
			return
		}
		active = settings
		reportError(nil)
	}

	reconcile()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return closeServer()
		case <-ticker.C:
			reconcile()
		}
	}
}
