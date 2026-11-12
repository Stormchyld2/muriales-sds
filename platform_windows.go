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
	profile := filepath.Join(os.Getenv("LOCALAPPDATA"), "MurialesSDSBook", "window-2") // new folder so a cached old icon is not reused
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

// freeSpace reports the free bytes on the drive holding path, or -1 if unknown.
func freeSpace(path string) int64 {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return -1
	}
	var free, total, totalFree uint64
	r, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW").Call(
		uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return -1
	}
	return int64(free)
}
