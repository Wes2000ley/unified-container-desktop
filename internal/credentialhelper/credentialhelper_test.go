package credentialhelper

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

type callerFunc func(context.Context, api.Request) (any, error)

func (f callerFunc) Call(ctx context.Context, request api.Request) (any, error) {
	return f(ctx, request)
}

func TestStoreGetEraseAndListUseControllerStore(t *testing.T) {
	const secret = "synthetic-test-token"
	var stored api.RegistryCredentialParams
	var methods []string
	caller := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		methods = append(methods, request.Method)
		if request.Method == "registry.credential.store" {
			if err := json.Unmarshal(request.Params, &stored); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		}
		if request.Method == "registry.credential.get" {
			return api.RegistryCredentialParams{ServerURL: "registry.example", Username: "test-user", Secret: secret}, nil
		}
		if request.Method == "registry.credential.list" {
			return map[string]string{"registry.example": "test-user"}, nil
		}
		return nil, nil
	})

	var stdout, stderr bytes.Buffer
	storeInput := `{"ServerURL":"registry.example","Username":"test-user","Secret":"` + secret + `"}`
	if code := Run(context.Background(), caller, []string{"store"}, strings.NewReader(storeInput), &stdout, &stderr); code != 0 {
		t.Fatalf("store exit=%d output=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stored.ServerURL != "registry.example" || stored.Username != "test-user" || stored.Secret != secret || stdout.Len() != 0 {
		t.Fatalf("store request=%#v output=%q", stored, stdout.String())
	}
	stdout.Reset()
	if code := Run(context.Background(), caller, []string{"get"}, strings.NewReader("registry.example\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("get exit=%d output=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var got credentials
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || got.Secret != secret || got.Username != "test-user" {
		t.Fatalf("get response=%#v err=%v", got, err)
	}
	stdout.Reset()
	if code := Run(context.Background(), caller, []string{"erase"}, strings.NewReader("registry.example\n"), &stdout, &stderr); code != 0 || stdout.Len() != 0 {
		t.Fatalf("erase exit=%d output=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := Run(context.Background(), caller, []string{"list"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("list exit=%d output=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var listed map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil || listed["registry.example"] != "test-user" {
		t.Fatalf("list response=%v err=%v", listed, err)
	}
	if strings.Join(methods, ",") != "registry.credential.store,registry.credential.get,registry.credential.erase,registry.credential.list" {
		t.Fatalf("methods=%v", methods)
	}
}

func TestGetMissingCredentialUsesDockerHelperError(t *testing.T) {
	caller := callerFunc(func(context.Context, api.Request) (any, error) {
		return nil, api.Fail("not_found", "internal detail without credentials")
	})
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), caller, []string{"get"}, strings.NewReader("registry.example"), &stdout, &stderr)
	if code != 1 || stdout.String() != "credentials not found in native keychain\n" || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestInputIsBoundedAndStrict(t *testing.T) {
	caller := callerFunc(func(context.Context, api.Request) (any, error) {
		t.Fatal("invalid input reached controller")
		return nil, nil
	})
	for _, input := range []string{
		`{"ServerURL":"registry.example","Username":"user","Secret":"secret"} {}`,
		`{"ServerURL":"registry.example","Username":"user","Secret":"secret","extra":true}`,
		strings.Repeat("x", maxInputBytes+1),
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), caller, []string{"store"}, strings.NewReader(input), &stdout, &stderr); code == 0 {
			t.Fatalf("accepted invalid input %q", input[:min(len(input), 60)])
		}
	}
}
