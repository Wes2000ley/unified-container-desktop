//go:build windows

package platform

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHelperStartupErrorIncludesBoundedJournalAndCause(t *testing.T) {
	m := testManager(t)
	var gotArgs []string
	startupMessage := "route ip+net: netlinkrib: address family not supported by protocol"
	m.runCommand = func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte(strings.Repeat("é", 4500) + "\n" + startupMessage + "\n"), nil
	}

	cause := errors.New("Linux helper readiness timed out")
	err := m.helperStartupError(context.Background(), cause)
	if !errors.Is(err, cause) {
		t.Fatalf("startup error %v lost readiness cause", err)
	}
	if want := []string{"--distribution", "UCD-Linux", "--user", "root", "--cd", "/", "--exec", "/usr/bin/journalctl", "-u", "ucd-helper.service", "-n", "20", "--no-pager", "--output=cat"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("journal command = %q, want %q", gotArgs, want)
	}

	const marker = "; helper startup journal:\n"
	index := strings.Index(err.Error(), marker)
	if index < 0 {
		t.Fatalf("startup journal missing from error: %v", err)
	}
	journal := err.Error()[index+len(marker):]
	if len(journal) > 8192 || !utf8.ValidString(journal) {
		t.Fatalf("journal tail is not valid UTF-8 within 8 KiB: bytes=%d valid=%t", len(journal), utf8.ValidString(journal))
	}
	if !strings.Contains(journal, startupMessage) {
		t.Fatalf("journal tail lost the startup cause: %q", journal)
	}
}

func TestHelperStartupErrorSkipsJournalWhenCanceled(t *testing.T) {
	m := testManager(t)
	calls := 0
	m.runCommand = func(context.Context, io.Reader, ...string) ([]byte, error) {
		calls++
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.helperStartupError(ctx, errors.New("Linux helper readiness timed out"))
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("startup error = %v, journal command calls = %d", err, calls)
	}
}

func TestHelperStartupErrorPreservesCauseWhenJournalFails(t *testing.T) {
	m := testManager(t)
	journalFailure := errors.New("journal service unavailable")
	m.runCommand = func(context.Context, io.Reader, ...string) ([]byte, error) {
		return nil, journalFailure
	}
	cause := errors.New("Linux helper readiness timed out")

	err := m.helperStartupError(context.Background(), cause)
	if !errors.Is(err, cause) {
		t.Fatalf("journal failure %v lost readiness cause", err)
	}
	if !strings.Contains(err.Error(), "read helper startup journal: "+journalFailure.Error()) {
		t.Fatalf("journal read failure was not reported: %v", err)
	}
}
