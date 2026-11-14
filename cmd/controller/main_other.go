//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "The Unified Container Desktop controller requires Windows 11 Pro or Enterprise.")
	os.Exit(1)
}
