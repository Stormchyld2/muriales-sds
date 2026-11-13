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

func freeSpace(string) int64 { return -1 }

func htmlToPDF(src, dst string) error { return os.ErrNotExist }

func openMailto(to []string, subject, body string) { fmt.Println("mailto", to, subject) }

func composeEmail(to []string, subject, body string) (string, string, bool) {
	fmt.Println("compose email", to, subject)
	return "outlook", "", false
}
