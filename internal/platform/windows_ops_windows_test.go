//go:build windows

package platform

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

type fakeEngineService struct {
	states              []svc.State
	calls               []string
	queryErr, actionErr error
}

func (f *fakeEngineService) Query() (svc.Status, error) {
	f.calls = append(f.calls, "query")
	if f.queryErr != nil {
		return svc.Status{}, f.queryErr
	}
	state := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return svc.Status{State: state}, nil
}
func (f *fakeEngineService) Start(...string) error {
	f.calls = append(f.calls, "start")
	return f.actionErr
}
func (f *fakeEngineService) Control(command svc.Cmd) (svc.Status, error) {
	f.calls = append(f.calls, "stop")
	if command != svc.Stop {
		panic("unexpected control")
	}
	return svc.Status{}, f.actionErr
}

func TestWindowsEngineWaitsForTransitions(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		states       []svc.State
		want         string
	}{
		{"start after pending stop", "start", []svc.State{svc.StopPending, svc.Stopped, svc.StartPending, svc.Running}, "query,query,start,query,query"},
		{"stop after pending start", "stop", []svc.State{svc.StartPending, svc.Running, svc.StopPending, svc.Stopped}, "query,query,stop,query,query"},
		{"start reports old stopped state", "start", []svc.State{svc.Stopped, svc.Stopped, svc.StartPending, svc.Running}, "query,start,query,query,query"},
		{"stop reports old running state", "stop", []svc.State{svc.Running, svc.Running, svc.StopPending, svc.Stopped}, "query,stop,query,query,query"},
		{"already running", "start", []svc.State{svc.Running}, "query"},
		{"already stopped", "stop", []svc.State{svc.Stopped}, "query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEngineService{states: tc.states}
			if err := setWindowsEngineState(context.Background(), f, tc.action, time.Microsecond); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(f.calls, ","); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestWindowsEngineDoesNotReportFalseSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		states       []svc.State
		actionErr    error
		contains     string
	}{
		{"already running error with stopped state", "start", []svc.State{svc.Stopped, svc.StopPending, svc.Stopped}, windows.ERROR_SERVICE_ALREADY_RUNNING, "did not start"},
		{"start terminates", "start", []svc.State{svc.Stopped, svc.StartPending, svc.Stopped}, nil, "did not start"},
		{"stop returns running", "stop", []svc.State{svc.Running, svc.StopPending, svc.Running}, nil, "did not stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEngineService{states: tc.states, actionErr: tc.actionErr}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			err := setWindowsEngineState(ctx, f, tc.action, time.Microsecond)
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestWindowsEngineErrorsAndCancellation(t *testing.T) {
	failure := errors.New("SCM error")
	for _, tc := range []struct {
		name string
		fake *fakeEngineService
	}{
		{"query", &fakeEngineService{queryErr: failure}},
		{"start", &fakeEngineService{states: []svc.State{svc.Stopped}, actionErr: failure}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := setWindowsEngineState(context.Background(), tc.fake, "start", time.Microsecond)
			if !errors.Is(err, failure) {
				t.Fatalf("got %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeEngineService{states: []svc.State{svc.StopPending}}
	if err := setWindowsEngineState(ctx, f, "start", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("called service after cancellation: %v", f.calls)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := setWindowsEngineState(ctx, f, "start", time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}
