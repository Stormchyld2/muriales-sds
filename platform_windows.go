//go:build windows

package main

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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

func msedgePath() string {
	for _, root := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LOCALAPPDATA")} {
		if root == "" {
			continue
		}
		p := filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// htmlToPDF has Edge print a saved page to a PDF, without opening a window.
func htmlToPDF(src, dst string) error {
	edge := msedgePath()
	if edge == "" {
		return os.ErrNotExist
	}
	profile := filepath.Join(os.TempDir(), "MurialesSDSBook-pdf")
	_ = os.MkdirAll(profile, 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	u := (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(src)}).String()
	cmd := exec.CommandContext(ctx, edge, "--headless=new", "--disable-gpu", "--no-first-run", "--user-data-dir="+profile,
		"--no-pdf-header-footer", "--print-to-pdf="+dst, u)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := cmd.Run(); err != nil {
		return err
	}
	if info, err := os.Stat(dst); err != nil || info.Size() == 0 {
		return os.ErrNotExist
	}
	return nil
}

// openOutlookDraft opens a new message in Outlook with the file attached. The
// message is shown, not sent. All text is passed through environment
// variables, never put into the script, so nothing typed can run as code.
func openOutlookDraft(to []string, subject, body, attach string) error {
	script := "$ErrorActionPreference='Stop'; $o=New-Object -ComObject Outlook.Application; $m=$o.CreateItem(0); " +
		"$m.To=$env:SDS_TO; $m.Subject=$env:SDS_SUBJECT; $m.Body=$env:SDS_BODY; $m.Attachments.Add($env:SDS_FILE) | Out-Null; $m.Display()"
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), "SDS_TO="+strings.Join(to, ";"), "SDS_SUBJECT="+subject, "SDS_BODY="+body, "SDS_FILE="+attach)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd.Run()
}

// openMailto opens the default mail program with a new message.
func openMailto(to []string, subject, body string) {
	u := "mailto:" + strings.Join(to, ";") + "?subject=" + strings.ReplaceAll(url.QueryEscape(subject), "+", "%20") + "&body=" + strings.ReplaceAll(url.QueryEscape(body), "+", "%20")
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}
