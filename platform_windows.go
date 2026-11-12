//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

// openWindow shows the program in its own application window (no address bar
// or tabs) using Microsoft Edge, which is part of Windows. Chrome is used if
// Edge is missing. As a last resort the default browser is opened.
func openWindow(url string) (*exec.Cmd, error) {
	var candidates []string
	for _, root := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LOCALAPPDATA")} {
		if root == "" {
			continue
		}
		candidates = append(candidates, filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"))
	}
	for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")} {
		if root == "" {
			continue
		}
		candidates = append(candidates, filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
	}
	profile := filepath.Join(os.Getenv("LOCALAPPDATA"), "MurialesSDSBook", "window")
	_ = os.MkdirAll(profile, 0o755)
	for _, exe := range candidates {
		if _, err := os.Stat(exe); err != nil {
			continue
		}
		cmd := exec.Command(exe,
			"--app="+url,
			"--user-data-dir="+profile,
			"--no-first-run",
			"--no-default-browser-check",
			"--window-size=1360,900",
		)
		if err := cmd.Start(); err == nil {
			return cmd, nil
		}
	}
	return nil, exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

func openFolder(path string) {
	_ = exec.Command("explorer.exe", path).Start()
}

func fatal(msg string) {
	text, _ := syscall.UTF16PtrFromString(msg)
	title, _ := syscall.UTF16PtrFromString("Muriale's SDS Book")
	syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(
		0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
	os.Exit(1)
}
