//go:build windows

package main

import (
	"context"
	"fmt"
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
	u := (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(src)}).String()
	var last error = os.ErrNotExist
	// Edge sometimes writes the PDF and then does not exit, so the file is
	// checked even when the program had to be stopped.
	for _, mode := range []string{"--headless=new", "--headless"} {
		_ = os.Remove(dst)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, edge, mode, "--disable-gpu", "--no-first-run", "--no-default-browser-check", "--user-data-dir="+profile,
			"--no-pdf-header-footer", "--virtual-time-budget=4000", "--print-to-pdf="+dst, u)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		err := cmd.Run()
		cancel()
		if info, serr := os.Stat(dst); serr == nil && info.Size() > 0 {
			return nil
		}
		if err != nil {
			last = err
		}
	}
	return last
}

// openOutlookDraft opens a new message in Outlook with the file attached. The
// message is shown, not sent. All text is passed through environment
// variables, never put into the script, so nothing typed can run as code.
func openOutlookDraft(to []string, subject, body, attach string) error {
	script := "$ErrorActionPreference='Stop'; $o=New-Object -ComObject Outlook.Application; $m=$o.CreateItem(0); " +
		"$m.To=$env:SDS_TO; $m.Subject=$env:SDS_SUBJECT; $m.Body=$env:SDS_BODY; $m.Attachments.Add($env:SDS_FILE) | Out-Null; $m.Display(); " +
		"try { $m.GetInspector.Activate() } catch {}; try { (New-Object -ComObject WScript.Shell).AppActivate($m.Subject) | Out-Null } catch {}"
	// Office installed as 32-bit cannot be reached from 64-bit PowerShell
	// (error 80040154), so the 32-bit PowerShell is tried as well.
	shells := []string{"powershell.exe"}
	if root := os.Getenv("SystemRoot"); root != "" {
		p := filepath.Join(root, "SysWOW64", "WindowsPowerShell", "v1.0", "powershell.exe")
		if _, err := os.Stat(p); err == nil {
			shells = append(shells, p)
		}
	}
	var last error
	for _, sh := range shells {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		cmd := exec.CommandContext(ctx, sh, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
		cmd.Env = append(os.Environ(), "SDS_TO="+strings.Join(to, ";"), "SDS_SUBJECT="+subject, "SDS_BODY="+body, "SDS_FILE="+attach)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		out, err := cmd.CombinedOutput()
		timedOut := ctx.Err() != nil
		cancel()
		if err == nil {
			return nil
		}
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if timedOut {
			return fmt.Errorf("Outlook took too long to answer")
		}
		if msg == "" {
			msg = err.Error()
		}
		last = fmt.Errorf("%s", msg)
		if !strings.Contains(msg, "80040154") {
			break
		}
	}
	return last
}

// copyFileToClipboard puts the file on the clipboard, so Ctrl+V in a message
// window attaches it.
func copyFileToClipboard(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", "Set-Clipboard -Path $env:SDS_FILE")
	cmd.Env = append(os.Environ(), "SDS_FILE="+path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd.Run() == nil
}

// outlookExe finds the classic Outlook program, which can start a message
// from the command line even when it cannot be driven from a script.
func outlookExe() string {
	for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if root == "" {
			continue
		}
		for _, v := range []string{"root\\Office16", "Office16", "root\\Office15", "Office15", "Office14"} {
			p := filepath.Join(root, "Microsoft Office", v, "OUTLOOK.EXE")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// composeEmail tries Outlook first, then Outlook's command line, then the
// default mail program. The second result explains why Outlook was skipped.
func composeEmail(to []string, subject, body, attach, dir string) (string, string, bool) {
	err := openOutlookDraft(to, subject, body, attach)
	if err == nil {
		return "outlook", "", false
	}
	detail := err.Error()
	if exe := outlookExe(); exe != "" {
		args := []string{"/c", "ipm.note", "/m", strings.Join(to, ";") + "?subject=" + strings.ReplaceAll(url.QueryEscape(subject), "+", "%%20"), "/a", attach}
		if exec.Command(exe, args...).Start() == nil {
			return "outlook", "", false
		}
	}
	short := body
	if len(short) > 1800 {
		short = short[:1800] + "..."
	}
	onClip := copyFileToClipboard(attach)
	writeEML(to, subject, body, attach, dir)
	openMailto(to, subject, short)
	openFolder(dir)
	return "mailto", detail, onClip
}

// openMailto opens the default mail program with a new message.
func openMailto(to []string, subject, body string) {
	u := "mailto:" + strings.Join(to, ";") + "?subject=" + strings.ReplaceAll(url.QueryEscape(subject), "+", "%20") + "&body=" + strings.ReplaceAll(url.QueryEscape(body), "+", "%20")
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
}
