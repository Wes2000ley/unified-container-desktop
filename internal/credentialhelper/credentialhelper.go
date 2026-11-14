// Package credentialhelper implements Docker's standard credential-helper
// stdin/stdout protocol for the machine-wide UCD registry store.
package credentialhelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/skyri/ucd/internal/api"
)

const maxInputBytes = 8 << 10

type credentials struct {
	ServerURL string `json:"ServerURL"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

// Run serves one Docker credential-helper command and returns its process exit
// code. Errors follow Docker's helper protocol: the message goes to stdout.
func Run(ctx context.Context, caller api.Caller, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if caller == nil {
		fmt.Fprintln(stdout, "UCD controller is unavailable")
		return 1
	}
	if len(args) != 1 {
		fmt.Fprintln(stdout, "Usage: docker-credential-ucd <store|get|erase|list|version>")
		return 1
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		fmt.Fprintf(stdout, "docker-credential-ucd %s\n", api.Version)
		return 0
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, "Usage: docker-credential-ucd <store|get|erase|list|version>")
		return 0
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintln(stdout, err)
		return 1
	}
	var request api.Request
	switch args[0] {
	case "store":
		var value credentials
		if err := decodeInput(stdin, &value); err != nil {
			fmt.Fprintln(stdout, err)
			return 1
		}
		if value.ServerURL == "" || value.Username == "" || value.Secret == "" {
			fmt.Fprintln(stdout, "registry server, username, and secret are required")
			return 1
		}
		request, _ = api.NewRequest("registry.credential.store", "", api.RegistryCredentialParams{
			ServerURL: value.ServerURL, Username: value.Username, Secret: value.Secret,
		})
	case "get", "erase":
		server, err := readServer(stdin)
		if err != nil {
			fmt.Fprintln(stdout, err)
			return 1
		}
		method := "registry.credential." + args[0]
		request, _ = api.NewRequest(method, "", api.RegistryCredentialParams{ServerURL: server})
	case "list":
		request = api.Request{Method: "registry.credential.list"}
	default:
		fmt.Fprintf(stdout, "unknown credential-helper command: %s\n", args[0])
		return 1
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, err := caller.Call(callCtx, request)
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Code == "not_found" && args[0] == "get" {
			fmt.Fprintln(stdout, "credentials not found in native keychain")
			return 1
		}
		fmt.Fprintln(stdout, err)
		return 1
	}
	if args[0] == "get" {
		var value credentials
		if err := decodeResult(result, &value); err != nil {
			fmt.Fprintf(stdout, "decode UCD registry credential: %v\n", err)
			return 1
		}
		if err := json.NewEncoder(stdout).Encode(value); err != nil {
			fmt.Fprintf(stderr, "write registry credential: %v\n", err)
			return 1
		}
	} else if args[0] == "list" {
		var values map[string]string
		if err := decodeResult(result, &values); err != nil {
			fmt.Fprintf(stdout, "decode UCD registry list: %v\n", err)
			return 1
		}
		if values == nil {
			values = map[string]string{}
		}
		if err := json.NewEncoder(stdout).Encode(values); err != nil {
			fmt.Fprintf(stderr, "write registry list: %v\n", err)
			return 1
		}
	}
	return 0
}

func readServer(input io.Reader) (string, error) {
	data, err := readInput(input)
	if err != nil {
		return "", err
	}
	server := strings.TrimSpace(string(data))
	if server == "" {
		return "", errors.New("registry server is required")
	}
	return server, nil
}

func decodeInput(input io.Reader, target any) error {
	data, err := readInput(input)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode Docker credential request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("Docker credential request must contain one JSON object")
	}
	return nil
}

func readInput(input io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Docker credential request: %w", err)
	}
	if len(data) > maxInputBytes {
		return nil, errors.New("Docker credential request exceeds 8 KiB")
	}
	return data, nil
}

func decodeResult(result any, target any) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
