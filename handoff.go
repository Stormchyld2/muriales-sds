package main

// The manager handoff report: the notes left for the next manager, which can
// be printed or emailed. Emailing saves the report (as a PDF when Edge can make
// one, otherwise as a web page) in the "Handoff Reports" folder beside the
// library and opens a draft in Outlook with it attached. The program never
// sends the message; the manager reads it and presses Send.

import (
	"encoding/json"
	"html"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var safeName = regexp.MustCompile(`[^A-Za-z0-9 _.-]+`)

func (s *store) handoffDir() string { return filepath.Join(filepath.Dir(s.dir), "Handoff Reports") }

type handoffRow struct{ date, from, note string }

// handoffRows lists the open notes, newest first. Callers hold s.mu.
func (s *store) handoffRows() []handoffRow {
	var out []handoffRow
	for i := len(s.lib.Notes) - 1; i >= 0; i-- {
		n := s.lib.Notes[i]
		if n.Done {
			continue
		}
		d := n.At
		if len(d) >= 10 {
			d = d[:10]
		}
		by := strings.TrimSpace(n.By)
		if by == "" {
			by = "A manager"
		}
		out = append(out, handoffRow{d, by, n.Text})
	}
	return out
}

func handoffHTML(rows []handoffRow, plain bool) string {
	e := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>Manager Handoff</title><style>
body{font:13px/1.4 Segoe UI,Arial,sans-serif;color:#111;margin:28px}h1{margin:0;font-size:22px}.sub{color:#555;margin-bottom:6px}.org{color:#7a1f2b;font-weight:700;font-size:12px;letter-spacing:.04em;text-transform:uppercase}
h2{font-size:15px;margin:20px 0 4px;border-bottom:1px solid #999;padding-bottom:2px}table{border-collapse:collapse;width:100%;margin:6px 0}th{background:#eee;text-align:left}th,td{border:1px solid #bbb;padding:4px 7px;vertical-align:top}
.empty{color:#666;font-style:italic;padding:8px 0}.btn{margin:10px 0;padding:6px 14px}@media print{.noprint{display:none}body{margin:12mm}tr{page-break-inside:avoid}}
</style></head><body>`)
	if !plain {
		b.WriteString(`<div class="noprint"><button class="btn" onclick="window.print()">Print</button></div>`)
	}
	b.WriteString(`<div class="org">Muriale's Italian Kitchen</div><h1>Manager Handoff</h1><div class="sub">` + e(time.Now().Format("Monday, January 2, 2006")) + `</div><h2>Notes for the next manager</h2>`)
	if len(rows) == 0 {
		b.WriteString(`<div class="empty">No open notes.</div>`)
	} else {
		b.WriteString("<table><thead><tr><th>Date</th><th>From</th><th>Note</th></tr></thead><tbody>")
		for _, r := range rows {
			b.WriteString("<tr><td>" + e(r.date) + "</td><td>" + e(r.from) + "</td><td>" + strings.ReplaceAll(e(r.note), "\n", "<br>") + "</td></tr>")
		}
		b.WriteString("</tbody></table>")
	}
	b.WriteString(`<div class="sub" style="margin-top:18px">Printed ` + time.Now().Format("1/2/2006 3:04 PM") + `</div></body></html>`)
	return b.String()
}

func handoffText(rows []handoffRow) string {
	var b strings.Builder
	b.WriteString("Manager Handoff\r\n" + time.Now().Format("Monday, January 2, 2006") + "\r\n\r\nNotes for the next manager\r\n")
	if len(rows) == 0 {
		b.WriteString("No open notes.\r\n")
	}
	for i, r := range rows {
		if i >= 25 {
			b.WriteString("... and more. See the attached report.\r\n")
			break
		}
		b.WriteString("- " + r.date + " | " + r.from + " | " + strings.ReplaceAll(r.note, "\n", " ") + "\r\n")
	}
	return b.String()
}

func (s *store) handoffRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/handoff/report", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		page := handoffHTML(s.handoffRows(), false)
		s.mu.Unlock()
		if r.URL.Query().Get("print") == "1" {
			page = strings.Replace(page, "</body>", `<script>onload=function(){setTimeout(function(){print()},300)}</script></body>`, 1)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	mux.HandleFunc("POST /api/handoff/email", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ To, Subject, Message string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			fail(w, 400, "Could not read that.")
			return
		}
		var addrs []string
		for _, a := range strings.Split(strings.NewReplacer(",", ";", "\n", ";").Replace(in.To), ";") {
			if a = strings.TrimSpace(a); a == "" {
				continue
			}
			p, err := mail.ParseAddress(a)
			if err != nil || strings.ContainsAny(a, "\r\n") {
				fail(w, 400, "“"+a+"” is not a valid email address.")
				return
			}
			addrs = append(addrs, p.Address)
		}
		if len(addrs) == 0 {
			fail(w, 400, "Enter who to send it to.")
			return
		}
		subject := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(in.Subject))
		if subject == "" {
			subject = "Manager Handoff - " + time.Now().Format("January 2, 2006")
		}
		if len(subject) > 200 || len(in.Message) > 4000 {
			fail(w, 400, "The subject or message is too long.")
			return
		}
		s.mu.Lock()
		rows := s.handoffRows()
		s.mu.Unlock()
		dir := s.handoffDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail(w, 500, "The reports folder could not be made: "+err.Error())
			return
		}
		file := filepath.Join(dir, strings.TrimSpace(safeName.ReplaceAllString("Manager Handoff "+time.Now().Format("2006-01-02 1504"), ""))+".html")
		if err := os.WriteFile(file, []byte(handoffHTML(rows, true)), 0o644); err != nil {
			fail(w, 500, "The report could not be saved: "+err.Error())
			return
		}
		attach := file
		pdfNote := ""
		if pdf := file[:len(file)-5] + ".pdf"; true {
			if perr := htmlToPDF(file, pdf); perr == nil {
				attach = pdf
			} else {
				pdfNote = perr.Error()
				s.logf("PDF could not be made: %v", perr)
			}
		}
		body := strings.TrimSpace(in.Message)
		if body != "" {
			body += "\r\n\r\n"
		}
		body += handoffText(rows)
		method, detail, onClip := composeEmail(addrs, subject, body, attach, dir)
		if detail != "" {
			s.logf("Outlook draft failed: %v", detail)
		}
		short := body
		if len(short) > 6000 {
			short = short[:6000]
		}
		writeJSON(w, 200, map[string]any{"method": method, "detail": detail, "text": short, "fileOnClipboard": onClip, "pdfNote": pdfNote, "file": filepath.Base(attach), "folder": dir})
	})
}
