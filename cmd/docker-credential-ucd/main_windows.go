//go:build windows

// Command docker-credential-ucd connects Docker CLI registry operations to the
// shared UCD machine registry store.
package main

import (
	"context"
	"os"

	"github.com/skyri/ucd/internal/credentialhelper"
	"github.com/skyri/ucd/internal/local"
)

func main() {
	os.Exit(run())
}

func run() int {
	client := local.NewClient()
	defer client.Close()
	return credentialhelper.Run(context.Background(), client, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}
