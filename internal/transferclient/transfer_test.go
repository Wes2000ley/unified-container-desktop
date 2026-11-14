package transferclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

type rawCaller struct {
	responses []json.RawMessage
	closed    bool
}

func (c *rawCaller) Call(ctx context.Context, request api.Request) (any, error) {
	params, err := api.Decode[api.TransferParams](request)
	if err != nil || request.Engine != "linux" || params.SessionID != "session" {
		return nil, errors.New("wrong transfer request")
	}
	switch request.Method {
	case "transfer.read":
		if params.MaxBytes != 256<<10 || len(c.responses) == 0 {
			return nil, errors.New("unexpected read")
		}
		result := c.responses[0]
		c.responses = c.responses[1:]
		return result, nil
	case "transfer.close":
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.closed = true
		return nil, nil
	default:
		return nil, errors.New("unexpected method")
	}
}

func TestCopyDecodesRawRPCChunksAndDataWithEOF(t *testing.T) {
	caller := &rawCaller{responses: []json.RawMessage{
		json.RawMessage(`{"data":"YWJj","eof":false}`),
		json.RawMessage(`{"data":"ZGVm","eof":true}`),
	}}
	var out bytes.Buffer
	n, err := Copy(context.Background(), caller, "linux", "session", &out)
	if err != nil || n != 6 || out.String() != "abcdef" {
		t.Fatalf("copy = %d %q, %v", n, out.String(), err)
	}
	if err := Close(caller, "linux", "session"); err != nil || !caller.closed {
		t.Fatalf("close = %v, closed %v", err, caller.closed)
	}
}

func TestCopyRejectsEmptyNonFinalChunk(t *testing.T) {
	caller := &rawCaller{responses: []json.RawMessage{json.RawMessage(`{"data":"","eof":false}`)}}
	if _, err := Copy(context.Background(), caller, "linux", "session", &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error for a response that cannot make progress")
	}
}
