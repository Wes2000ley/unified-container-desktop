//go:build windows

package platform

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestLocalAccountSIDMatchesCurrentLocalIdentity(t *testing.T) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	account, domain, _, err := user.User.Sid.LookupAccount("")
	if err != nil {
		t.Fatal(err)
	}
	var computer [16]uint16
	n := uint32(len(computer))
	if err := syscall.GetComputerName(&computer[0], &n); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(domain, syscall.UTF16ToString(computer[:n])) {
		t.Skip("current process identity is not a local account")
	}
	sid, err := localAccountSID(account)
	if err != nil {
		t.Fatal(err)
	}
	if !sid.Equals(user.User.Sid) {
		t.Fatal("qualified local account resolved to a different SID")
	}
}

func TestLocalAccountSIDReportsLookupFailure(t *testing.T) {
	_, err := localAccountSID("UCD-Account-That-Does-Not-Exist-9A61E20D")
	if err == nil || !errors.Is(err, windows.ERROR_NONE_MAPPED) {
		t.Fatalf("expected account lookup failure, got %v", err)
	}
}
