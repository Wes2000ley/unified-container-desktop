package dockerproxy

import (
	"errors"
	"io"
	"testing"
)

func TestUpgradeBodyCloseWrite(t *testing.T) {
	closedPipe := &testUpgradeBody{closeWriteErr: io.ErrClosedPipe}
	if err := (upgradeBody{ReadWriteCloser: closedPipe}).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite on an already-finished stream = %v, want nil", err)
	}

	underlying := errors.New("write half-close failed")
	failedWrite := &testUpgradeBody{closeWriteErr: underlying}
	err := (upgradeBody{ReadWriteCloser: failedWrite}).CloseWrite()
	if !errors.Is(err, underlying) {
		t.Fatalf("CloseWrite error = %v, want wrapped cause %v", err, underlying)
	}

	if err := (upgradeBody{ReadWriteCloser: testUpgradeBodyWithoutCloseWrite{}}).CloseWrite(); err == nil {
		t.Fatal("CloseWrite without underlying support succeeded")
	}
}

type testUpgradeBody struct {
	closeWriteErr error
}

func (b *testUpgradeBody) Read([]byte) (int, error)    { return 0, io.EOF }
func (b *testUpgradeBody) Write(p []byte) (int, error) { return len(p), nil }
func (b *testUpgradeBody) Close() error                { return nil }
func (b *testUpgradeBody) CloseWrite() error           { return b.closeWriteErr }

type testUpgradeBodyWithoutCloseWrite struct{}

func (testUpgradeBodyWithoutCloseWrite) Read([]byte) (int, error)    { return 0, io.EOF }
func (testUpgradeBodyWithoutCloseWrite) Write(p []byte) (int, error) { return len(p), nil }
func (testUpgradeBodyWithoutCloseWrite) Close() error                { return nil }
