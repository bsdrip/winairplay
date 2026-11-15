//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "The winairplay tray application runs only on Windows. Use winairplay-cli on this system.")
	os.Exit(1)
}
