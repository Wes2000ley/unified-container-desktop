// Command ucd controls the machine-wide Unified Container Desktop service.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"strings"

	"github.com/skyri/ucd/internal/cli"
	"github.com/skyri/ucd/internal/local"
	"github.com/skyri/ucd/internal/webui"
)

func main() { os.Exit(run()) }

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := local.NewClient()
	defer client.Close()
	if isDockerEntrypoint(os.Args[0]) {
		return cli.RunDocker(ctx, client, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	}
	if len(os.Args) > 1 && os.Args[1] == "web" {
		flags := flag.NewFlagSet("web", flag.ContinueOnError)
		flags.SetOutput(os.Stderr)
		port := flags.Int("port", 0, "loopback port (default: automatically selected)")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return 1
		}
		if flags.NArg() != 0 {
			fmt.Fprintln(os.Stderr, "ucd web: unexpected arguments")
			return 1
		}
		if err := webui.Run(ctx, client, *port, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "ucd web:", err)
			return 1
		}
		return 0
	}
	return cli.Run(ctx, client, os.Args[1:], os.Stdout, os.Stderr)
}

func isDockerEntrypoint(executable string) bool {
	base := path.Base(strings.ReplaceAll(executable, `\`, "/"))
	return strings.EqualFold(base, "docker") || strings.EqualFold(base, "docker.exe")
}
