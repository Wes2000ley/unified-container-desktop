//go:build windows

package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/skyri/ucd/internal/api"
	"golang.org/x/sys/windows/svc"
)

// RunService runs under the Windows service manager when launched by it, or
// in the foreground otherwise. Stop and shutdown cancel the supplied context.
func RunService(run func(context.Context) error) error {
	if run == nil {
		return errors.New("service run function is required")
	}
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("detect Windows service session: %w", err)
	}
	if !isService {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return run(ctx)
	}
	h := &serviceHandler{run: run, result: make(chan error, 1)}
	if err := svc.Run(api.ServiceName, h); err != nil {
		return fmt.Errorf("run Windows service: %w", err)
	}
	select {
	case err := <-h.result:
		return err
	default:
		return errors.New("Windows service dispatcher exited before the service completed")
	}
}

type serviceHandler struct {
	run    func(context.Context) error
	result chan error
}

func (h *serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	statuses <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			h.result <- err
			if err != nil {
				return true, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				statuses <- svc.Status{State: svc.StopPending, WaitHint: 20000}
				cancel()
				err := <-done
				if errors.Is(err, context.Canceled) {
					err = nil
				}
				h.result <- err
				if err != nil {
					return true, 1
				}
				return false, 0
			}
		}
	}
}
