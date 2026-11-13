package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const appVersion = "1.6.6"

// ---------------------------------------------------------------- notes

// Note is a handoff note one manager leaves for the next. Notes are never
// deleted; finished ones are marked done and kept as a record.
type Note struct {
	ID     string `json:"id"`
	At     string `json:"at"`
	By     string `json:"by"`
	Text   string `json:"text"`
	Done   bool   `json:"done"`
	DoneAt string `json:"doneAt"`
	DoneBy string `json:"doneBy"`
}

// ------------------------------------------------------ file fingerprints

type hashEntry struct {
	size int64
	mod  time.Time
	sum  string
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sumBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fileStatus compares a product's PDF with the fingerprint taken when it was
// added. The program never writes to a stored PDF, so anything other than
// "ok" means the file was changed or damaged outside the program.
func (s *store) fileStatus(p *Product, full bool) string {
	path := s.pdfPath(p)
	info, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	if p.SHA256 == "" {
		return "ok"
	}
	if s.hc == nil {
		s.hc = map[string]hashEntry{}
	}
	e, ok := s.hc[path]
	if !full && ok && e.size == info.Size() && e.mod.Equal(info.ModTime()) {
		if e.sum == p.SHA256 {
			return "ok"
		}
		return "changed"
	}
	sum, err := hashFile(path)
	if err != nil {
		return "missing"
	}
	s.hc[path] = hashEntry{info.Size(), info.ModTime(), sum}
	if sum == p.SHA256 {
		return "ok"
	}
	return "changed"
}

type integrityRow struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	File   string `json:"file"`
	Status string `json:"status"` // ok, changed or missing
}

func (s *store) integrity(full bool) []integrityRow {
	rows := make([]integrityRow, 0, len(s.lib.Products))
	for _, p := range s.lib.Products {
		rows = append(rows, integrityRow{p.ID, p.Name, filepath.Base(s.pdfPath(p)), s.fileStatus(p, full)})
	}
	return rows
}

// integrityProblems lists only the products whose file is not as it was added.
func (s *store) integrityProblems() map[string]string {
	out := map[string]string{}
	for _, r := range s.integrity(false) {
		if r.Status != "ok" {
			out[r.ID] = r.Status
		}
	}
	return out
}

// ----------------------------------------------------------------- helpers

var titleCleaner = strings.NewReplacer("_", " ", "-", " ")

func titleFromFile(name string) string {
	t := strings.Join(strings.Fields(titleCleaner.Replace(strings.TrimSuffix(name, filepath.Ext(name)))), " ")
	if t == "" {
		t = "Unnamed SDS"
	}
	return t
}

// csvCell stops a spreadsheet from running a cell as a formula.
func csvCell(v string) string {
	if v != "" && strings.ContainsAny(v[:1], "=+-@\t\r") {
		return "'" + v
	}
	return v
}

func zipTo(w io.Writer, src string) error {
	zw := zip.NewWriter(w)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(path, ".tmp") || strings.HasSuffix(path, ".log") {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		zf, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(zf, f)
		return err
	})
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	return err
}

func download(w http.ResponseWriter, contentType, name string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
}

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn or bad
	Detail string `json:"detail"`
}

func (s *store) newestBackup() (string, time.Time) {
	entries, _ := os.ReadDir(s.backupDir())
	var name string
	var when time.Time
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".zip") && info.ModTime().After(when) {
			name, when = e.Name(), info.ModTime()
		}
	}
	return name, when
}

func logTail(path string, lines int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) > 16<<10 {
		b = b[len(b)-16<<10:]
	}
	all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

func (s *store) diagnostics() map[string]any {
	var checks []check
	add := func(name, status, detail string) { checks = append(checks, check{name, status, detail}) }

	add("Library file", "ok", fmt.Sprintf("library.json opened (format %d): %d products, %d removed.", s.lib.Schema, len(s.lib.Products), len(s.lib.Removed)))
	probe := filepath.Join(s.dir, ".write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		add("Data folder", "bad", "The program cannot save files in the data folder: "+err.Error())
	} else {
		os.Remove(probe)
		add("Data folder", "ok", "The data folder can be written to.")
	}
	if free := freeSpace(s.dir); free >= 0 {
		gb := float64(free) / (1 << 30)
		st := "ok"
		if free < 1<<30 {
			st = "warn"
		}
		add("Free disk space", st, fmt.Sprintf("%.1f GB free on the drive holding the library.", gb))
	}
	missing, changed := 0, 0
	for _, r := range s.integrity(false) {
		switch r.Status {
		case "missing":
			missing++
		case "changed":
			changed++
		}
	}
	if missing > 0 {
		add("Sheets present", "bad", fmt.Sprintf("%d product(s) have no PDF in the sheets folder.", missing))
	} else {
		add("Sheets present", "ok", "Every product has its PDF.")
	}
	if changed > 0 {
		add("Document integrity", "bad", fmt.Sprintf("%d PDF(s) differ from the fingerprint taken when they were added.", changed))
	} else {
		add("Document integrity", "ok", "Every PDF matches the fingerprint taken when it was added.")
	}
	review, unverified := 0, 0
	for _, p := range s.lib.Products {
		if p.NeedsReview {
			review++
		}
		if !p.hasSummary() || p.SummaryNeedsCheck || p.SummaryVerifiedAt == "" {
			unverified++
		}
	}
	st := "ok"
	if review > 0 {
		st = "warn"
	}
	add("Needs review", st, fmt.Sprintf("%d product(s) are waiting for their details.", review))
	st = "ok"
	if unverified > 0 {
		st = "warn"
	}
	add("Emergency summaries", st, fmt.Sprintf("%d product(s) have no verified emergency summary.", unverified))
	if name, when := s.newestBackup(); name == "" {
		add("Backups", "warn", "No backup has been made yet.")
	} else if age := time.Since(when); age > 30*24*time.Hour {
		add("Backups", "warn", fmt.Sprintf("The newest backup is %d days old (%s).", int(age.Hours()/24), name))
	} else {
		add("Backups", "ok", fmt.Sprintf("Newest backup: %s.", name))
	}
	if exists(s.file() + ".bak") {
		add("Previous save", "ok", "A copy of the previous save is kept (library.json.bak).")
	} else {
		add("Previous save", "warn", "No previous-save copy exists yet.")
	}
	open := 0
	for _, n := range s.lib.Notes {
		if !n.Done {
			open++
		}
	}
	add("Handoff notes", "ok", fmt.Sprintf("%d open note(s).", open))

	exe, _ := os.Executable()
	tail := logTail(filepath.Join(s.dir, "sdsbook.log"), 30)
	var rep strings.Builder
	fmt.Fprintf(&rep, "Muriale's SDS Manager diagnostics\nVersion: %s\nChecked: %s\nSystem: %s/%s, %s\nProgram: %s\nLibrary folder: %s\nBackups folder: %s\n\n",
		appVersion, time.Now().Format("2006-01-02 15:04:05"), runtime.GOOS, runtime.GOARCH, runtime.Version(), exe, s.dir, s.backupDir())
	for _, c := range checks {
		fmt.Fprintf(&rep, "[%s] %s: %s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
	}
	if tail != "" {
		fmt.Fprintf(&rep, "\nRecent program log:\n%s\n", tail)
	}
	return map[string]any{"version": appVersion, "checks": checks, "report": rep.String(), "program": exe, "folder": s.dir, "backups": s.backupDir()}
}

// recoverPreviousSave puts back the copy of library.json kept from the save
// before the current one. The library being replaced is kept beside it.
func (s *store) recoverPreviousSave() (string, error) {
	bak := s.file() + ".bak"
	b, err := os.ReadFile(bak)
	if err != nil {
		return "", errors.New("there is no previous-save copy to go back to")
	}
	var lib Library
	if err := json.Unmarshal(b, &lib); err != nil {
		return "", errors.New("the previous-save copy is damaged, so it was not used")
	}
	keep := s.file() + ".replaced-" + time.Now().Format("2006-01-02-150405")
	if exists(s.file()) {
		if err := copyFile(s.file(), keep); err != nil {
			return "", fmt.Errorf("the current library could not be kept safe, so nothing was changed: %v", err)
		}
	}
	tmp := s.file() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, s.file()); err != nil {
		return "", err
	}
	previous := s.lib
	if err := s.load(); err != nil {
		s.lib = previous
		if exists(keep) {
			_ = copyFile(keep, s.file())
		}
		return "", fmt.Errorf("the previous save did not open correctly, so it was put back as it was: %v", err)
	}
	return keep, nil
}

var archiveSuffix = regexp.MustCompile(`_\d{4}-\d{2}-\d{2}_\d{6}_[0-9a-f]{6}$`)

// ------------------------------------------------------------------ routes

func (s *store) moreRoutes(mux *http.ServeMux) {
	s.extractRoutes(mux)
	s.locationRoutes(mux)
	s.purgeRoutes(mux)
	// ---- handoff notes
	s.handoffRoutes(mux)
	mux.HandleFunc("POST /api/notes", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			By   string `json:"by"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			fail(w, 400, "the note could not be read")
			return
		}
		text := strings.TrimSpace(in.Text)
		if text == "" || len(text) > 2000 {
			fail(w, 400, "write a note of up to 2000 characters")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		old := s.lib.Notes
		n := &Note{ID: newID(), At: now(), By: strings.TrimSpace(in.By), Text: text}
		s.lib.Notes = append(append([]*Note{}, old...), n)
		if err := s.save(); err != nil {
			s.lib.Notes = old
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, n)
	})
	mux.HandleFunc("POST /api/notes/{id}/{what}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			By string `json:"by"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
		s.mu.Lock()
		defer s.mu.Unlock()
		var n *Note
		for _, q := range s.lib.Notes {
			if q.ID == r.PathValue("id") {
				n = q
			}
		}
		if n == nil {
			fail(w, 404, "that note no longer exists")
			return
		}
		before := *n
		switch r.PathValue("what") {
		case "done":
			n.Done, n.DoneAt, n.DoneBy = true, now(), strings.TrimSpace(in.By)
		case "reopen":
			n.Done, n.DoneAt, n.DoneBy = false, "", ""
		default:
			fail(w, 404, "unknown action")
			return
		}
		if err := s.save(); err != nil {
			*n = before
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, n)
	})

	// ---- document integrity
	mux.HandleFunc("GET /api/integrity", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"rows": s.integrity(r.URL.Query().Get("full") == "1"), "checked": now()})
	})
	// A manager accepts the file as it is now. The binder then shows that
	// sheet as needing to be replaced, since the paper copy came from the old file.
	mux.HandleFunc("POST /api/integrity/{id}/accept", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		p := s.find(r.PathValue("id"))
		if p == nil {
			fail(w, 404, "that product is no longer in the library")
			return
		}
		sum, err := hashFile(s.pdfPath(p))
		if err != nil {
			fail(w, 400, "the PDF cannot be read, so it cannot be accepted")
			return
		}
		before := *p
		p.SHA256, p.PDFUpdatedAt, p.UpdatedAt = sum, now(), now()
		if err := s.save(); err != nil {
			*p = before
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("POST /api/products/{id}/pages", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Pages int `json:"pages"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in); err != nil || in.Pages < 1 || in.Pages > 5000 {
			fail(w, 400, "that is not a page count")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if p := s.find(r.PathValue("id")); p != nil && p.Pages != in.Pages {
			old := p.Pages
			p.Pages = in.Pages
			if err := s.save(); err != nil {
				p.Pages = old
			}
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	// ---- export
	mux.HandleFunc("GET /api/export/products.csv", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		download(w, "text/csv; charset=utf-8", "Muriales SDS Products "+time.Now().Format("2006-01-02")+".csv")
		_, _ = io.WriteString(w, "\xEF\xBB\xBF") // so Excel reads accents correctly
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"Product Name", "Also Known As", "Manufacturer", "Supplier", "Category", "Also Used For", "Storage Location",
			"SDS Revision Date", "Signal Word", "Summary Verified On", "Summary Verified By", "PDF File", "Pages", "SHA-256"})
		for _, p := range s.lib.Products {
			_ = cw.Write([]string{csvCell(p.Name), csvCell(p.Aliases), csvCell(p.Manufacturer), csvCell(p.Supplier), csvCell(p.Category), csvCell(p.Tags),
				csvCell(p.Location), p.RevisionDate, p.SignalWord, p.SummaryVerifiedAt, csvCell(p.SummaryVerifiedBy), filepath.Base(s.pdfPath(p)),
				fmt.Sprint(p.Pages), p.SHA256})
		}
		cw.Flush()
	})
	mux.HandleFunc("GET /api/export/library.zip", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		download(w, "application/zip", "Muriales SDS Library "+time.Now().Format("2006-01-02")+".zip")
		if err := zipTo(w, s.dir); err != nil {
			s.logf("library export failed: %v", err)
		}
	})

	// ---- import
	mux.HandleFunc("POST /api/import/pdfs", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			fail(w, 400, "the files could not be read; they may be too large")
			return
		}
		files := r.MultipartForm.File["pdf"]
		if len(files) == 0 || len(files) > 500 {
			fail(w, 400, "choose between 1 and 500 PDF files")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		known := map[string]string{}
		for _, p := range s.lib.Products {
			if p.SHA256 != "" {
				known[p.SHA256] = p.Name
			}
		}
		var made []*Product
		var written []string
		type skip struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		}
		skipped := []skip{}
		for _, fh := range files {
			name := filepath.Base(fh.Filename)
			f, err := fh.Open()
			if err != nil {
				skipped = append(skipped, skip{name, "could not be read"})
				continue
			}
			tmp, err := os.CreateTemp(s.pdfDir(), "import-*.tmp")
			if err != nil {
				f.Close()
				skipped = append(skipped, skip{name, "could not be saved"})
				continue
			}
			h := sha256.New()
			head := make([]byte, 1024)
			n, _ := io.ReadFull(f, head)
			isPDF := strings.Contains(string(head[:n]), "%PDF")
			_, _ = tmp.Write(head[:n])
			h.Write(head[:n])
			_, cerr := io.Copy(io.MultiWriter(tmp, h), f)
			f.Close()
			tmp.Close()
			sum := hex.EncodeToString(h.Sum(nil))
			switch {
			case cerr != nil:
				os.Remove(tmp.Name())
				skipped = append(skipped, skip{name, "could not be saved"})
			case !isPDF:
				os.Remove(tmp.Name())
				skipped = append(skipped, skip{name, "is not a PDF"})
			case known[sum] != "":
				os.Remove(tmp.Name())
				skipped = append(skipped, skip{name, "is already in the library as " + known[sum]})
			default:
				title := titleFromFile(name)
				draft := draftFile(tmp.Name())
				if draft != nil && draft.Name != "" {
					title = draft.Name
				}
				p := &Product{ID: newID(), Name: title, Category: catchAll, NeedsReview: true, SHA256: sum,
					AddedAt: now(), UpdatedAt: now(), PDFUpdatedAt: now()}
				if draft != nil {
					applyDraft(p, draft)
				}
				p.File = s.fileNameFor(title)
				if err := os.Rename(tmp.Name(), s.pdfPath(p)); err != nil {
					os.Remove(tmp.Name())
					skipped = append(skipped, skip{name, "could not be saved"})
					continue
				}
				known[sum] = title
				written = append(written, s.pdfPath(p))
				made = append(made, p)
			}
		}
		if len(made) > 0 {
			old := s.lib.Products
			s.lib.Products = append(append([]*Product{}, old...), made...)
			if err := s.save(); err != nil {
				s.lib.Products = old
				for _, f := range written {
					os.Remove(f)
				}
				fail(w, 500, "the library could not be saved, so nothing was added: "+err.Error())
				return
			}
		}
		names := []string{}
		for _, p := range made {
			names = append(names, p.Name)
		}
		writeJSON(w, 200, map[string]any{"added": names, "skipped": skipped})
	})
	// A backup zip from anywhere (an export, a USB drive) is placed with the
	// other backups; restoring it is then the same checked, undoable restore.
	mux.HandleFunc("POST /api/import/backup", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			fail(w, 400, "the file could not be read; it may be too large")
			return
		}
		f, fh, err := r.FormFile("zip")
		if err != nil {
			fail(w, 400, "choose a library zip file")
			return
		}
		defer f.Close()
		if err := os.MkdirAll(s.backupDir(), 0o755); err != nil {
			fail(w, 500, "the backups folder could not be created")
			return
		}
		base := strings.TrimSuffix(strings.Join(strings.Fields(badFileChars.ReplaceAllString(filepath.Base(fh.Filename), " ")), " "), ".zip")
		if len(base) > 60 {
			base = base[:60]
		}
		name := "Imported " + time.Now().Format("2006-01-02 150405") + " " + strings.TrimSpace(base) + ".zip"
		path := filepath.Join(s.backupDir(), name)
		out, err := os.Create(path)
		if err == nil {
			_, err = io.Copy(out, f)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			os.Remove(path)
			fail(w, 500, "the file could not be saved: "+err.Error())
			return
		}
		if _, err := inspectBackup(path); err != nil {
			os.Remove(path)
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"file": name})
	})

	// ---- diagnostics
	mux.HandleFunc("GET /api/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, 200, s.diagnostics())
	})

	// ---- recovery
	mux.HandleFunc("GET /api/recovery", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := map[string]any{"backups": s.backupDir()}
		prev := map[string]any{"exists": false}
		if info, err := os.Stat(s.file() + ".bak"); err == nil {
			prev["exists"], prev["modified"] = true, info.ModTime().Format("2006-01-02T15:04:05")
			if b, err := os.ReadFile(s.file() + ".bak"); err == nil {
				var lib Library
				if json.Unmarshal(b, &lib) == nil {
					prev["products"], prev["removed"] = len(lib.Products), len(lib.Removed)
				} else {
					prev["damaged"] = true
				}
			}
		}
		out["previousSave"] = prev
		linked := map[string]bool{}
		for _, p := range s.lib.Removed {
			linked[p.ArchivedFile] = true
		}
		unlinked := []map[string]any{}
		if entries, err := os.ReadDir(s.goneDir()); err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".pdf") || linked[e.Name()] {
					continue
				}
				info, _ := e.Info()
				unlinked = append(unlinked, map[string]any{"file": e.Name(), "title": titleFromFile(archiveSuffix.ReplaceAllString(strings.TrimSuffix(e.Name(), ".pdf"), "")), "modified": info.ModTime().Format("2006-01-02T15:04:05")})
			}
		}
		sort.Slice(unlinked, func(i, j int) bool { return unlinked[i]["modified"].(string) > unlinked[j]["modified"].(string) })
		out["unlinked"] = unlinked
		held := []string{}
		if entries, err := os.ReadDir(s.backupDir()); err == nil {
			for _, e := range entries {
				if e.IsDir() && strings.HasPrefix(e.Name(), "Before restore ") {
					held = append(held, e.Name())
				}
			}
		}
		out["beforeRestore"] = held
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/recovery/previous-save", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		keep, err := s.recoverPreviousSave()
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"products": len(s.lib.Products), "keptAs": filepath.Base(keep)})
	})
	mux.HandleFunc("POST /api/recovery/rebuild", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		before := len(s.lib.Products)
		changed := s.scan()
		if changed {
			if err := s.save(); err != nil {
				fail(w, 500, "the library could not be saved: "+err.Error())
				return
			}
		}
		missing := 0
		for _, p := range s.lib.Products {
			if p.Missing {
				missing++
			}
		}
		writeJSON(w, 200, map[string]int{"added": len(s.lib.Products) - before, "missing": missing, "products": len(s.lib.Products)})
	})
	mux.HandleFunc("POST /api/recovery/unlinked", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			File string `json:"file"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&in); err != nil || in.File == "" || in.File != filepath.Base(in.File) {
			fail(w, 400, "choose a file from the list")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		src := filepath.Join(s.goneDir(), in.File)
		if !exists(src) {
			fail(w, 404, "that file is no longer in the removed folder")
			return
		}
		sum, err := hashFile(src)
		if err != nil {
			fail(w, 500, "the file could not be read")
			return
		}
		title := titleFromFile(archiveSuffix.ReplaceAllString(strings.TrimSuffix(in.File, ".pdf"), ""))
		p := &Product{ID: newID(), Name: title, Category: catchAll, NeedsReview: true, SHA256: sum, AddedAt: now(), UpdatedAt: now(), PDFUpdatedAt: now()}
		p.File = s.fileNameFor(title)
		if err := os.Rename(src, s.pdfPath(p)); err != nil {
			fail(w, 500, "the file could not be moved back: "+err.Error())
			return
		}
		old := s.lib.Products
		s.lib.Products = append(append([]*Product{}, old...), p)
		if err := s.save(); err != nil {
			s.lib.Products = old
			_ = os.Rename(s.pdfPath(p), src)
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, p)
	})
}

// applyDraft fills the empty fields of a product from a drafted summary. It never overwrites what a
// manager typed, and never marks anything verified.
func applyDraft(p *Product, d *Draft) {
	fill := func(dst *string, v string) {
		if strings.TrimSpace(*dst) == "" {
			*dst = v
		}
	}
	fill(&p.Manufacturer, d.Manufacturer)
	fill(&p.RevisionDate, d.RevisionDate)
	fill(&p.SignalWord, d.SignalWord)
	fill(&p.Hazards, d.Hazards)
	fill(&p.FirstAidEyes, d.FirstAidEyes)
	fill(&p.FirstAidSkin, d.FirstAidSkin)
	fill(&p.FirstAidInhalation, d.FirstAidInhalation)
	fill(&p.FirstAidSwallowed, d.FirstAidSwallowed)
	fill(&p.PPE, d.PPE)
	if len(p.Pictograms) == 0 {
		p.Pictograms = d.Pictograms
	}
}

// draftFile reads a PDF on disk and drafts its summary; nil when it cannot be read.
func draftFile(path string) *Draft {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	d, err := extractDraft(b)
	if err != nil {
		return nil
	}
	return d
}

func (s *store) extractRoutes(mux *http.ServeMux) {
	reply := func(w http.ResponseWriter, b []byte) {
		d, err := extractDraft(b)
		if err != nil {
			writeJSON(w, 200, map[string]any{"unreadable": "The summary could not be read from this PDF: " + err.Error() + ". Type it from Sections 2, 4 and 8."})
			return
		}
		writeJSON(w, 200, d)
	}
	// A PDF that has not been saved yet (the Add screen).
	mux.HandleFunc("POST /api/extract", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			fail(w, 400, "the file could not be read")
			return
		}
		f, _, err := r.FormFile("pdf")
		if err != nil {
			fail(w, 400, "choose a PDF first")
			return
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 64<<20))
		if err != nil {
			fail(w, 400, "the file could not be read")
			return
		}
		reply(w, b)
	})
	// The PDF already stored for a product.
	mux.HandleFunc("POST /api/products/{id}/extract", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		p := s.find(r.PathValue("id"))
		var path string
		if p != nil {
			path = s.pdfPath(p)
		}
		s.mu.Unlock()
		if p == nil {
			fail(w, 404, "that product was not found")
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			fail(w, 404, "the PDF for this product is missing")
			return
		}
		reply(w, b)
	})
}

// locationRoutes lets a manager keep the list of storage areas.
func (s *store) locationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/locations", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Locations []string          `json:"locations"`
			Renames   map[string]string `json:"renames"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			fail(w, 400, "the storage areas could not be read")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		seen := map[string]bool{}
		var list []string
		for _, l := range in.Locations {
			l = strings.Join(strings.Fields(l), " ")
			if l != "" && len(l) <= 60 && !seen[strings.ToLower(l)] {
				seen[strings.ToLower(l)] = true
				list = append(list, l)
			}
		}
		// Work out each product's new area first, so a refused change touches nothing.
		next := map[*Product]string{}
		inUse := map[string]int{}
		for _, p := range s.lib.Products {
			cur := strings.TrimSpace(p.Location)
			if to, ok := in.Renames[cur]; ok {
				cur = strings.Join(strings.Fields(to), " ")
				if cur != "" && !seen[strings.ToLower(cur)] {
					seen[strings.ToLower(cur)] = true
					list = append(list, cur)
				}
			}
			if cur != "" && !seen[strings.ToLower(cur)] {
				inUse[cur]++
			}
			next[p] = cur
		}
		if len(inUse) > 0 {
			var names []string
			for n, c := range inUse {
				names = append(names, fmt.Sprintf("%s (%d)", n, c))
			}
			sort.Strings(names)
			fail(w, 409, "These storage areas still have products in them: "+strings.Join(names, ", ")+". Move or rename them first.")
			return
		}
		oldList, old := s.lib.Locations, map[*Product]string{}
		for p, loc := range next {
			if p.Location != loc {
				old[p] = p.Location
				p.Location = loc
			}
		}
		s.lib.Locations = list
		if err := s.save(); err != nil {
			s.lib.Locations = oldList
			for p, loc := range old {
				p.Location = loc
			}
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
}

// removedKeepDays is how long a removed product and its PDF are kept before being deleted for good.
const removedKeepDays = 30

func removedAge(p *Product) (time.Duration, bool) {
	t, err := time.ParseInLocation("2006-01-02T15:04:05", p.RemovedAt, time.Local)
	if err != nil {
		return 0, false
	}
	return time.Since(t), true
}

// purgeOld permanently deletes removed products, and PDFs left in the removed folder, that are older
// than removedKeepDays. The caller holds s.mu. It does nothing in a read-only copy.
func (s *store) purgeOld() (products, files int) {
	if s.isReadOnly() {
		return
	}
	limit := time.Duration(removedKeepDays) * 24 * time.Hour
	var keep []*Product
	var doomed []string
	for _, p := range s.lib.Removed {
		if age, ok := removedAge(p); ok && age > limit {
			if p.ArchivedFile != "" {
				doomed = append(doomed, filepath.Join(s.goneDir(), p.ArchivedFile))
			}
			products++
			continue
		}
		keep = append(keep, p)
	}
	linked := map[string]bool{}
	for _, p := range keep {
		linked[p.ArchivedFile] = true
	}
	if entries, err := os.ReadDir(s.goneDir()); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".pdf") || linked[e.Name()] {
				continue
			}
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > limit {
				doomed = append(doomed, filepath.Join(s.goneDir(), e.Name()))
			}
		}
	}
	if products > 0 {
		old := s.lib.Removed
		s.lib.Removed = keep
		if err := s.save(); err != nil {
			s.lib.Removed = old
			return 0, 0
		}
	}
	for _, f := range doomed {
		if os.Remove(f) == nil {
			files++
		}
	}
	if products+files > 0 {
		s.logf("deleted for good after %d days: %d removed products, %d PDFs", removedKeepDays, products, files)
	}
	return
}

func (s *store) purgeRoutes(mux *http.ServeMux) {
	// A manager deletes one removed product for good, before its 30 days are up.
	mux.HandleFunc("DELETE /api/removed/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := r.PathValue("id")
		var keep []*Product
		var gone *Product
		for _, p := range s.lib.Removed {
			if p.ID == id {
				gone = p
			} else {
				keep = append(keep, p)
			}
		}
		if gone == nil {
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		old := s.lib.Removed
		s.lib.Removed = keep
		if err := s.save(); err != nil {
			s.lib.Removed = old
			fail(w, 500, "the library could not be saved, so nothing was deleted: "+err.Error())
			return
		}
		if gone.ArchivedFile != "" {
			_ = os.Remove(filepath.Join(s.goneDir(), filepath.Base(gone.ArchivedFile)))
		}
		s.logf("deleted for good: %s", gone.Name)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
}
