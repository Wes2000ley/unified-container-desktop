package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

type registryCaller func(context.Context, api.Request) (any, error)

func (f registryCaller) Call(ctx context.Context, request api.Request) (any, error) {
	return f(ctx, request)
}

func TestNativeRegistryCommandsAreGlobalAndReadPasswordFromStdin(t *testing.T) {
	const secret = "synthetic-native-cli-secret"
	cases := []struct {
		args   []string
		stdin  string
		method string
		server string
	}{
		{args: []string{"list"}, method: "registry.list"},
		{args: []string{"login", "--server", "registry.example", "--username", "cli-user", "--password-stdin"}, stdin: secret + "\n", method: "registry.login", server: "registry.example"},
		{args: []string{"logout", "--server", "registry.example"}, method: "registry.logout", server: "registry.example"},
	}
	for _, test := range cases {
		t.Run(test.method, func(t *testing.T) {
			called := false
			caller := registryCaller(func(_ context.Context, request api.Request) (any, error) {
				called = true
				if request.Method != test.method || request.Engine != "" {
					t.Fatalf("request=%#v", request)
				}
				if test.method == "registry.list" {
					return []map[string]string{{"server": "registry.example", "username": "cli-user"}}, nil
				}
				var params api.RegistryParams
				if err := json.Unmarshal(request.Params, &params); err != nil {
					t.Fatal(err)
				}
				if params.Server != test.server {
					t.Fatalf("server=%q", params.Server)
				}
				if test.method == "registry.login" && (params.Username != "cli-user" || params.Password != secret) {
					t.Fatalf("login params=%#v", params)
				}
				return nil, nil
			})
			var stdout, stderr bytes.Buffer
			code := runRegistry(context.Background(), caller, test.args, strings.NewReader(test.stdin), &stdout, &stderr)
			if code != 0 || !called {
				t.Fatalf("exit=%d called=%v stdout=%q stderr=%q", code, called, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
				t.Fatal("registry command printed its password")
			}
		})
	}
}

func TestRegistryLoginRejectsUnsafePasswordInput(t *testing.T) {
	caller := registryCaller(func(context.Context, api.Request) (any, error) {
		t.Fatal("invalid login reached the controller")
		return nil, nil
	})
	for _, input := range []string{"", "first\nsecond\n", strings.Repeat("x", 4097)} {
		var stdout, stderr bytes.Buffer
		code := runRegistry(context.Background(), caller, []string{"login", "--username", "test", "--password-stdin"}, strings.NewReader(input), &stdout, &stderr)
		if code == 0 {
			t.Fatalf("accepted unsafe password input of %d bytes", len(input))
		}
	}
}
