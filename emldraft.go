package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeEML saves the message as an unsent draft file (.eml) with the report
// attached. Mail programs that open it as a draft let the manager just press
// Send. It is a spare route for Outlook versions that cannot be scripted.
func writeEML(to []string, subject, body, attach, dir string) string {
	data, err := os.ReadFile(attach)
	if err != nil {
		return ""
	}
	ctype := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(attach)) {
	case ".pdf":
		ctype = "application/pdf"
	case ".html":
		ctype = "text/html"
	}
	clean := func(v string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(v) }
	var b bytes.Buffer
	boundary := fmt.Sprintf("=_draft_%d", time.Now().UnixNano())
	fmt.Fprintf(&b, "X-Unsent: 1\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", clean(strings.Join(to, "; ")), mime.QEncoding.Encode("utf-8", clean(subject)), boundary)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", boundary)
	writeB64(&b, []byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")))
	name := filepath.Base(attach)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; name=\"%s\"\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=\"%s\"\r\n\r\n", boundary, ctype, name, name)
	writeB64(&b, data)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	path := filepath.Join(dir, strings.TrimSuffix(name, filepath.Ext(name))+" (email draft).eml")
	if os.WriteFile(path, b.Bytes(), 0o644) != nil {
		return ""
	}
	return path
}

func writeB64(b *bytes.Buffer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
}
