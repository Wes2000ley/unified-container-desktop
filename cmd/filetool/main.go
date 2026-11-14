package main

import (
	"os"

	"github.com/skyri/ucd/internal/filetool"
)

func main() {
	os.Exit(filetool.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
