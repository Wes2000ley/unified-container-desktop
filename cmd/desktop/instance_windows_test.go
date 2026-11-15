//go:build windows

package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func instanceTestIdentity(t *testing.T) (context.Context, string, string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx, fmt.Sprintf(`\\.\pipe\UCD.Desktop.Test.%d`, time.Now().UnixNano()), user.User.Sid.String()
}

func TestDesktopConcurrentLaunchesShareOneInstance(t *testing.T) {
	ctx, name, sid := instanceTestIdentity(t)
	owner, err := claimDesktopInstance(ctx, name, sid, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	var workers sync.WaitGroup
	for range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			other, err := claimDesktopInstance(ctx, name, sid, false, false)
			if err != nil {
				t.Error(err)
			}
			if other != nil {
				other.close()
				t.Error("duplicate desktop acquired ownership")
			}
		}()
	}
	workers.Wait()
	select {
	case <-owner.show:
	case <-ctx.Done():
		t.Fatal("repeated launch did not activate existing window")
	}
	other, err := claimDesktopInstance(ctx, name, sid, false, true)
	if err != nil || other != nil {
		t.Fatalf("background launch: owner=%v error=%v", other, err)
	}
	select {
	case <-owner.show:
		t.Fatal("startup raised an already running window")
	default:
	}
}

func TestDesktopAdministratorTakeoverReleasesOldInstance(t *testing.T) {
	ctx, name, sid := instanceTestIdentity(t)
	owner, err := claimDesktopInstance(ctx, name, sid, false, false)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		select {
		case <-owner.quit:
		case <-ctx.Done():
		}
		owner.close()
	}()
	elevated, err := claimDesktopInstance(ctx, name, sid, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if elevated == nil {
		t.Fatal("administrator did not acquire GUI after handoff")
	}
	defer elevated.close()
	<-closed
	for _, admin := range []bool{false, true} {
		other, err := claimDesktopInstance(ctx, name, sid, admin, false)
		if err != nil || other != nil {
			t.Fatalf("elevated instance duplicated: %v %v", other, err)
		}
	}
	select {
	case <-elevated.quit:
		t.Fatal("existing administrator GUI was told to exit")
	default:
	}
}

func TestDesktopInstanceCanReopenAfterExit(t *testing.T) {
	ctx, name, sid := instanceTestIdentity(t)
	owner, err := claimDesktopInstance(ctx, name, sid, false, false)
	if err != nil {
		t.Fatal(err)
	}
	owner.close()
	reopened, err := claimDesktopInstance(ctx, name, sid, false, false)
	if err != nil || reopened == nil {
		t.Fatalf("could not reopen: %v", err)
	}
	reopened.close()
}
