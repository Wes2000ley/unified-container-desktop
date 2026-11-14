//go:build windows

package local

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestServiceHandlerStopCancelsAndWaits(t *testing.T) {
	started := make(chan struct{})
	h := &serviceHandler{
		run: func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		result: make(chan error, 1),
	}
	requests := make(chan svc.ChangeRequest)
	statuses := make(chan svc.Status, 4)
	finished := make(chan uint32, 1)
	go func() { _, code := h.Execute(nil, requests, statuses); finished <- code }()
	if status := <-statuses; status.State != svc.StartPending {
		t.Fatalf("first status = %+v", status)
	}
	if status := <-statuses; status.State != svc.Running || status.Accepts&svc.AcceptStop == 0 {
		t.Fatalf("running status = %+v", status)
	}
	<-started
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	if status := <-statuses; status.State != svc.StopPending {
		t.Fatalf("stop status = %+v", status)
	}
	select {
	case code := <-finished:
		if code != 0 || <-h.result != nil {
			t.Fatal("normal service stop reported failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service did not stop")
	}
}

func TestServiceHandlerReportsRunFailure(t *testing.T) {
	want := errors.New("run failed")
	h := &serviceHandler{run: func(context.Context) error { return want }, result: make(chan error, 1)}
	statuses := make(chan svc.Status, 2)
	specific, code := h.Execute(nil, make(chan svc.ChangeRequest), statuses)
	if !specific || code == 0 || !errors.Is(<-h.result, want) {
		t.Fatal("service did not report run failure")
	}
}
