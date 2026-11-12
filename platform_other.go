//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// Used only for testing on non-Windows machines.
func openWindow(url string) (*exec.Cmd, error) {
	fmt.Println("open", url)
	return nil, nil
}

func openFolder(path string) { fmt.Println("open folder", path) }

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
