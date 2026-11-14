package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/skyri/ucd/internal/api"
)

func runRegistry(ctx context.Context, caller api.Caller, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "ucd: usage: ucd registry ls|login|logout [options]")
		return 1
	}
	action := args[0]
	if action == "ls" {
		action = "list"
	}
	if action != "list" && action != "login" && action != "logout" {
		fmt.Fprintf(stderr, "ucd: unknown registry action %q\n", action)
		return 1
	}
	f := flags("registry " + action)
	server := f.String("server", "", "")
	username := f.String("username", "", "")
	passwordStdin := f.Bool("password-stdin", false, "")
	if err := f.Parse(args[1:]); err != nil {
		fmt.Fprintln(stderr, "ucd: registry options:", err)
		return 1
	}
	if f.NArg() != 0 {
		fmt.Fprintln(stderr, "ucd: registry command does not accept positional arguments")
		return 1
	}
	params := api.RegistryParams{Server: *server}
	if action == "list" {
		if *server != "" || *username != "" || *passwordStdin {
			fmt.Fprintln(stderr, "ucd: registry list does not accept options")
			return 1
		}
	} else if action == "login" {
		if *username == "" || !*passwordStdin {
			fmt.Fprintln(stderr, "ucd: registry login requires --username and --password-stdin")
			return 1
		}
		password, err := readRegistryPassword(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "ucd: read registry password:", err)
			return 1
		}
		params.Username, params.Password = *username, password
	} else if *username != "" || *passwordStdin {
		fmt.Fprintln(stderr, "ucd: registry logout accepts only --server")
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	result, err := call(ctx, caller, invocation{method: "registry." + action, params: params})
	if err != nil {
		fmt.Fprintln(stderr, "ucd:", err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		fmt.Fprintln(stderr, "ucd: write registry result:", err)
		return 1
	}
	return 0
}

func readRegistryPassword(input io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(input, 4097))
	if err != nil {
		return "", fmt.Errorf("read standard input: %w", err)
	}
	if len(data) > 4096 {
		return "", errors.New("password or token exceeds 4096 bytes")
	}
	password := strings.TrimSuffix(string(data), "\n")
	password = strings.TrimSuffix(password, "\r")
	if strings.ContainsAny(password, "\r\n") {
		return "", errors.New("standard input must contain one password or token line")
	}
	if password == "" {
		return "", errors.New("password or token is empty")
	}
	return password, nil
}
