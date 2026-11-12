package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

type env struct {
	t *testing.T
	s *store
	h http.Handler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s := &store{dir: filepath.Join(t.TempDir(), "SDS Data")}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.routes(mux)
	return &env{t, s, mux}
}

func pdf(body string) []byte { return []byte("%PDF-1.4\n" + body + "\n%%EOF\n") }

// send posts a product form and returns the status and decoded reply.
func (e *env) send(path string, meta map[string]any, file []byte) (int, map[string]any) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	m, _ := json.Marshal(meta)
	_ = mw.WriteField("meta", string(m))
	if file != nil {
		fw, _ := mw.CreateFormFile("pdf", "sheet.pdf")
		_, _ = fw.Write(file)
	}
	mw.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return e.do(req)
}

func (e *env) do(req *http.Request) (int, map[string]any) {
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *env) post(path, body string) (int, map[string]any) {
	return e.do(httptest.NewRequest("POST", path, bytes.NewBufferString(body)))
}

func (e *env) add(name string, body string, extra map[string]any) string {
	e.t.Helper()
	meta := map[string]any{"name": name, "category": "Degreasers"}
	for k, v := range extra {
		meta[k] = v
	}
	code, out := e.send("/api/products", meta, pdf(body))
	if code != 200 {
		e.t.Fatalf("add %s: %d %v", name, code, out)
	}
	return out["id"].(string)
}

func (e *env) read(p *Product) string {
	b, err := os.ReadFile(e.s.pdfPath(p))
	if err != nil {
		e.t.Fatalf("reading %s: %v", p.Name, err)
	}
	return string(b)
}

func files(t *testing.T, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestStarterSheetLoadsUnverified(t *testing.T) {
	e := newEnv(t)
	p := e.s.find("keystone-greaselift-rtu")
	if p == nil || !exists(e.s.pdfPath(p)) {
		t.Fatal("starter sheet was not loaded")
	}
	if p.SummaryVerifiedAt != "" {
		t.Error("a starter summary must not count as verified until a manager checks it")
	}
}

func TestReplaceArchivesOldSheetAndFlagsSummary(t *testing.T) {
	e := newEnv(t)
	id := e.add("Oven Cleaner", "version one", map[string]any{
		"hazards": "Corrosive", "revisionDate": "2024-01-01", "verify": true, "verifiedBy": "MM",
	})
	p := e.s.find(id)
	if p.SummaryVerifiedAt == "" || p.SummaryVerifiedBy != "MM" || p.SummarySourceRevision != "2024-01-01" {
		t.Fatalf("summary should be verified on add: %+v", p)
	}
	code, out := e.send("/api/products/"+id, map[string]any{"name": "Oven Cleaner", "category": "Degreasers", "hazards": "Corrosive", "revisionDate": "2026-01-01"}, pdf("version two"))
	if code != 200 {
		t.Fatalf("replace failed: %d %v", code, out)
	}
	if got := e.read(p); got != string(pdf("version two")) {
		t.Errorf("new sheet not in place: %q", got)
	}
	if !p.SummaryNeedsCheck {
		t.Error("replacing the PDF must mark a verified summary as needing a re-check")
	}
	old := files(t, e.s.goneDir())
	if len(old) != 1 {
		t.Fatalf("expected the old sheet in the removed folder, found %v", old)
	}
	if b, _ := os.ReadFile(filepath.Join(e.s.goneDir(), old[0])); string(b) != string(pdf("version one")) {
		t.Error("archived sheet is not the original")
	}
	// Verifying again clears the flag and records the new revision.
	e.send("/api/products/"+id, map[string]any{"name": "Oven Cleaner", "category": "Degreasers", "hazards": "Corrosive", "revisionDate": "2026-01-01", "verify": true, "verifiedBy": "MM"}, nil)
	if p.SummaryNeedsCheck || p.SummarySourceRevision != "2026-01-01" {
		t.Errorf("re-verification not recorded: %+v", p)
	}
	// Editing the text without verifying takes verification away.
	e.send("/api/products/"+id, map[string]any{"name": "Oven Cleaner", "category": "Degreasers", "hazards": "Corrosive. Wear gloves."}, nil)
	if p.SummaryVerifiedAt != "" {
		t.Error("an edited summary must not stay verified")
	}
}

func TestFailedReplaceLeavesOriginalUntouched(t *testing.T) {
	e := newEnv(t)
	id := e.add("Fryer Cleaner", "original", map[string]any{"location": "Dish Room"})
	p := e.s.find(id)
	// Make archiving impossible: the removed folder becomes a plain file.
	if err := os.RemoveAll(e.s.goneDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.s.goneDir(), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _ := e.send("/api/products/"+id, map[string]any{"name": "Renamed", "category": "Laundry"}, pdf("replacement"))
	if code != 500 {
		t.Fatalf("expected the replace to be refused, got %d", code)
	}
	if got := e.read(p); got != string(pdf("original")) {
		t.Errorf("original sheet was disturbed: %q", got)
	}
	if p.Name != "Fryer Cleaner" || p.Location != "Dish Room" {
		t.Errorf("product details changed despite the failure: %+v", p)
	}
	for _, n := range files(t, e.s.pdfDir()) {
		if filepath.Ext(n) != ".pdf" {
			t.Errorf("left-over file in sheets folder: %s", n)
		}
	}
}

func TestRemoveKeepsRecordAndRestoreBringsItBack(t *testing.T) {
	e := newEnv(t)
	id := e.add("Glass Cleaner", "glass", map[string]any{"manufacturer": "Diversey", "location": "Janitorial Closet"})
	if code, _ := e.do(httptest.NewRequest("DELETE", "/api/products/"+id, nil)); code != 200 {
		t.Fatal("remove failed")
	}
	if e.s.find(id) != nil {
		t.Fatal("product still active after removal")
	}
	if len(e.s.lib.Removed) != 1 || e.s.lib.Removed[0].Manufacturer != "Diversey" || e.s.lib.Removed[0].ArchivedFile == "" {
		t.Fatalf("removed record not kept: %+v", e.s.lib.Removed)
	}
	if code, out := e.post("/api/removed/"+id+"/restore", ""); code != 200 {
		t.Fatalf("restore failed: %d %v", code, out)
	}
	p := e.s.find(id)
	if p == nil || p.Location != "Janitorial Closet" || len(e.s.lib.Removed) != 0 {
		t.Fatalf("product not restored with its details: %+v", p)
	}
	if got := e.read(p); got != string(pdf("glass")) {
		t.Errorf("restored sheet content wrong: %q", got)
	}
}

func TestFolderDropNeedsReviewUntilCompleted(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.s.pdfDir(), "Drain_Opener-SDS.pdf"), pdf("drain"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.do(httptest.NewRequest("GET", "/api/library", nil))
	var p *Product
	for _, q := range e.s.lib.Products {
		if q.File == "Drain_Opener-SDS.pdf" {
			p = q
		}
	}
	if p == nil || !p.NeedsReview {
		t.Fatalf("dropped PDF should become a product needing review: %+v", p)
	}
	e.send("/api/products/"+p.ID, map[string]any{"name": "Drain Opener", "category": "Drain Maintenance"}, nil)
	if !p.NeedsReview {
		t.Error("still incomplete, should stay in review")
	}
	e.send("/api/products/"+p.ID, map[string]any{"name": "Drain Opener", "category": "Drain Maintenance", "manufacturer": "Ecolab", "location": "Dish Room", "revisionDate": "2025-05-01"}, nil)
	if p.NeedsReview {
		t.Error("completed details should clear the review flag")
	}
}

func TestBackupAndRestoreRoundTrip(t *testing.T) {
	e := newEnv(t)
	a := e.add("Product A", "aaa", map[string]any{"location": "Dish Room"})
	b := e.add("Product B", "bbb", nil)
	code, out := e.post("/api/backup", "")
	if code != 200 {
		t.Fatalf("backup failed: %v", out)
	}
	backup := filepath.Base(out["file"].(string))

	// Change the library after the backup.
	e.do(httptest.NewRequest("DELETE", "/api/products/"+a, nil))
	e.send("/api/products/"+b, map[string]any{"name": "Product B", "category": "Laundry"}, pdf("bbb changed"))
	c := e.add("Product C", "ccc", nil)

	code, out = e.post("/api/backups/inspect", `{"file":"`+backup+`"}`)
	if code != 200 || out["products"].(float64) != 3 || len(out["missingPdfs"].([]any)) != 0 {
		t.Fatalf("inspect: %d %v", code, out)
	}
	code, out = e.post("/api/restore", `{"file":"`+backup+`"}`)
	if code != 200 {
		t.Fatalf("restore failed: %v", out)
	}
	if e.s.find(c) != nil {
		t.Error("product added after the backup should be gone")
	}
	pa, pb := e.s.find(a), e.s.find(b)
	if pa == nil || pb == nil || pa.Location != "Dish Room" || pb.Category != "Degreasers" {
		t.Fatalf("restored products wrong: %+v %+v", pa, pb)
	}
	if e.read(pa) != string(pdf("aaa")) || e.read(pb) != string(pdf("bbb")) {
		t.Error("restored sheets do not match the backup")
	}
	// The restored library must open cleanly from disk, as at program start.
	fresh := &store{dir: e.s.dir}
	if err := fresh.load(); err != nil || len(fresh.lib.Products) != 3 {
		t.Fatalf("restored library did not reopen: %v", err)
	}
	// The library that was replaced is kept, not deleted.
	held := out["previousKeptIn"].(string)
	if !exists(filepath.Join(held, "library.json")) || !exists(filepath.Join(held, "sheets", "Product C.pdf")) {
		t.Errorf("previous library not kept in %s", held)
	}
}

func TestRestoreRejectsBadFiles(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(e.s.backupDir(), 0o755)
	_ = os.WriteFile(filepath.Join(e.s.backupDir(), "junk.zip"), []byte("not a zip"), 0o644)
	before := len(e.s.lib.Products)
	for _, name := range []string{"junk.zip", "../SDS Data/library.json", "missing.zip"} {
		if code, _ := e.post("/api/restore", `{"file":"`+name+`"}`); code == 200 {
			t.Errorf("restore accepted %q", name)
		}
	}
	if len(e.s.lib.Products) != before || !exists(e.s.file()) {
		t.Error("library disturbed by a refused restore")
	}
}

func TestBinderGeneratedIsNotConfirmed(t *testing.T) {
	e := newEnv(t)
	a := e.add("Sanitizer", "s1", nil)
	b := e.add("Degreaser", "d1", nil)
	ids := `["keystone-greaselift-rtu","` + a + `","` + b + `"]`

	// An incomplete book can be recorded but never confirmed.
	e.post("/api/binder/generated", `{"scope":"all","pages":20,"complete":false,"missing":["Sanitizer"],"ids":`+ids+`}`)
	if code, _ := e.post("/api/binder/confirm", ""); code == 200 {
		t.Fatal("an incomplete book must not be confirmable")
	}
	// A complete book is recorded, and the binder is untouched until confirmed.
	e.post("/api/binder/generated", `{"scope":"all","pages":30,"complete":true,"missing":[],"ids":`+ids+`}`)
	if e.s.lib.Binder != nil {
		t.Fatal("generating a book must not change the binder record")
	}
	if code, out := e.post("/api/binder/confirm", ""); code != 200 {
		t.Fatalf("confirm failed: %d %v", code, out)
	}
	bd := e.s.lib.Binder
	if len(bd.Items) != 3 || bd.Pages != 30 || bd.UpdatedAt == "" || !e.s.lib.LastBuild.Confirmed {
		t.Fatalf("binder not recorded: %+v", bd)
	}
	if bd.Items[a].PDFUpdatedAt != e.s.find(a).PDFUpdatedAt {
		t.Error("binder record does not match the product")
	}

	// Replace one PDF and print only that sheet: just it is merged in.
	e.send("/api/products/"+a, map[string]any{"name": "Sanitizer", "category": "Degreasers"}, pdf("s2"))
	was := bd.Items[b]
	e.post("/api/binder/generated", `{"scope":"selected","pages":3,"complete":true,"missing":[],"ids":["`+a+`"]}`)
	e.post("/api/binder/confirm", "")
	if e.s.lib.Binder.Items[a].PDFUpdatedAt != e.s.find(a).PDFUpdatedAt || e.s.lib.Binder.Items[b] != was {
		t.Error("a selected-sheets print should update only those sheets")
	}

	// A removed product stays in the binder record until the pages are taken out.
	e.do(httptest.NewRequest("DELETE", "/api/products/"+b, nil))
	if _, ok := e.s.lib.Binder.Items[b]; !ok {
		t.Fatal("removed product should still be listed as in the binder")
	}
	e.post("/api/binder/pulled", "")
	if _, ok := e.s.lib.Binder.Items[b]; ok {
		t.Error("taken-out sheet should leave the binder record")
	}
	// An index-only print cannot start a binder record.
	e.s.lib.Binder = nil
	e.post("/api/binder/generated", `{"scope":"index","pages":1,"complete":true,"missing":[],"ids":`+ids+`}`)
	if code, _ := e.post("/api/binder/confirm", ""); code == 200 {
		t.Error("an index alone must not start a binder record")
	}
}

func TestMoveLibraryToAnotherFolder(t *testing.T) {
	e := newEnv(t)
	e.s.cfg = filepath.Join(t.TempDir(), "settings.json")
	id := e.add("Rinse Aid", "rinse", nil)
	old := e.s.dir
	target := filepath.Join(t.TempDir(), "OneDrive - Muriales", "SDS Data")
	body, _ := json.Marshal(map[string]string{"path": target})
	code, out := e.post("/api/data-folder", string(body))
	if code != 200 || out["copied"] != true {
		t.Fatalf("move failed: %d %v", code, out)
	}
	p := e.s.find(id)
	if e.s.dir != target || p == nil || e.read(p) != string(pdf("rinse")) {
		t.Fatal("library not usable in its new folder")
	}
	if !exists(filepath.Join(old, "library.json")) {
		t.Error("the old folder must be left in place")
	}
	var st settings
	b, _ := os.ReadFile(e.s.cfg)
	if json.Unmarshal(b, &st) != nil || st.DataFolder != target {
		t.Errorf("new location not remembered: %s", b)
	}
	// Pointing back at the old folder opens the library already there, without copying over it.
	body, _ = json.Marshal(map[string]string{"path": old})
	if code, out = e.post("/api/data-folder", string(body)); code != 200 || out["copied"] != false {
		t.Fatalf("switching to an existing library: %d %v", code, out)
	}
	// A relative path or a folder inside the library is refused.
	for _, bad := range []string{"SDS", filepath.Join(old, "sheets", "x")} {
		body, _ = json.Marshal(map[string]string{"path": bad})
		if code, _ = e.post("/api/data-folder", string(body)); code == 200 {
			t.Errorf("accepted bad folder %q", bad)
		}
	}
}

// ---------------------------------------------------------------- new features

func (e *env) get(path string) (*httptest.ResponseRecorder, map[string]any) {
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (e *env) multi(path, field string, files map[string][]byte) (int, map[string]any) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, data := range files {
		fw, _ := mw.CreateFormFile(field, name)
		_, _ = fw.Write(data)
	}
	mw.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return e.do(req)
}

func TestFingerprintDetectsOutsideChanges(t *testing.T) {
	e := newEnv(t)
	id := e.add("Rinse Aid", "original", nil)
	p := e.s.find(id)
	if p.SHA256 != sumBytes(pdf("original")) {
		t.Fatalf("fingerprint not taken on add: %q", p.SHA256)
	}
	if got := e.s.integrityProblems(); len(got) != 0 {
		t.Fatalf("a fresh library should have no integrity problems: %v", got)
	}
	// Someone edits the stored file outside the program.
	f, _ := os.OpenFile(e.s.pdfPath(p), os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("tampered")
	f.Close()
	_, lib := e.get("/api/library")
	if lib["integrity"].(map[string]any)[id] != "changed" {
		t.Fatalf("outside edit not reported: %v", lib["integrity"])
	}
	// Accepting records the new fingerprint, and the binder will show the sheet for replacement.
	p.PDFUpdatedAt = "2000-01-01T00:00:00" // timestamps are to the second; make the change visible
	was := p.PDFUpdatedAt
	if code, out := e.post("/api/integrity/"+id+"/accept", ""); code != 200 {
		t.Fatalf("accept: %d %v", code, out)
	}
	if len(e.s.integrityProblems()) != 0 || p.SHA256 == sumBytes(pdf("original")) || p.PDFUpdatedAt == was {
		t.Errorf("accept did not re-baseline: %+v", p)
	}
	// Replacing through the program takes a new fingerprint.
	e.send("/api/products/"+id, map[string]any{"name": "Rinse Aid", "category": "Dish Machine"}, pdf("new sheet"))
	if p.SHA256 != sumBytes(pdf("new sheet")) {
		t.Error("replace did not take a new fingerprint")
	}
	// A file that goes missing is reported as missing.
	os.Remove(e.s.pdfPath(p))
	if e.s.integrityProblems()[id] != "missing" {
		t.Error("a deleted file should be reported missing")
	}
}

func TestAliasesAndSearchFields(t *testing.T) {
	e := newEnv(t)
	id := e.add("Heavy Duty Degreaser", "deg", map[string]any{"aliases": "Sysco Classic Degreaser, HD Degreaser"})
	if e.s.find(id).Aliases != "Sysco Classic Degreaser, HD Degreaser" {
		t.Error("aliases not saved on add")
	}
	e.send("/api/products/"+id, map[string]any{"name": "Heavy Duty Degreaser", "category": "Degreasers", "aliases": "Kitchen Degreaser"}, nil)
	if e.s.find(id).Aliases != "Kitchen Degreaser" {
		t.Error("aliases not saved on edit")
	}
}

func TestHandoffNotes(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.post("/api/notes", `{"by":"MM","text":"   "}`); code == 200 {
		t.Fatal("an empty note must be refused")
	}
	code, out := e.post("/api/notes", `{"by":"MM","text":"New fryer cleaner arrives Friday; replace the Keystone sheet."}`)
	if code != 200 {
		t.Fatalf("add note: %v", out)
	}
	id := out["id"].(string)
	e.post("/api/notes/"+id+"/done", `{"by":"JR"}`)
	n := e.s.lib.Notes[0]
	if !n.Done || n.DoneBy != "JR" || n.DoneAt == "" {
		t.Fatalf("done not recorded: %+v", n)
	}
	e.post("/api/notes/"+id+"/reopen", `{}`)
	if e.s.lib.Notes[0].Done || len(e.s.lib.Notes) != 1 {
		t.Error("reopen should clear done and keep the note")
	}
	if code, _ := e.post("/api/notes/"+id+"/delete", `{}`); code == 200 {
		t.Error("notes must not be deletable")
	}
}

func TestExports(t *testing.T) {
	e := newEnv(t)
	e.add("=HYPERLINK(\"http://evil\")", "x", map[string]any{"manufacturer": "-cmd", "aliases": "Plain, name"})
	rec, _ := e.get("/api/export/products.csv")
	body := rec.Body.String()
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte("\xEF\xBB\xBF")) || rec.Header().Get("Content-Disposition") == "" {
		t.Error("csv should be a download with a byte-order mark for Excel")
	}
	if bytes.Contains([]byte(body), []byte("\n=HYPERLINK")) || !bytes.Contains([]byte(body), []byte("'=HYPERLINK")) || !bytes.Contains([]byte(body), []byte("'-cmd")) {
		t.Errorf("cells that start like formulas must be defused:\n%s", body)
	}
	rec, _ = e.get("/api/export/library.zip")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" || rec.Body.Len() < 100 {
		t.Fatalf("library zip export failed: %d", rec.Code)
	}
}

func TestImportPDFsAndLibraryZip(t *testing.T) {
	e := newEnv(t)
	e.add("Existing Product", "same bytes", nil)
	code, out := e.multi("/api/import/pdfs", "pdf", map[string][]byte{
		"Floor_Stripper-SDS.pdf": pdf("floor"),
		"Duplicate.pdf":          pdf("same bytes"),
		"notes.pdf":              []byte("this is text"),
	})
	if code != 200 || len(out["added"].([]any)) != 1 || len(out["skipped"].([]any)) != 2 {
		t.Fatalf("import: %d %v", code, out)
	}
	var imported *Product
	for _, p := range e.s.lib.Products {
		if p.Name == "Floor Stripper SDS" {
			imported = p
		}
	}
	if imported == nil || !imported.NeedsReview || imported.SHA256 != sumBytes(pdf("floor")) || e.read(imported) != string(pdf("floor")) {
		t.Fatalf("imported product wrong: %+v", imported)
	}
	// Export the library, then import that zip as a backup and restore it.
	rec, _ := e.get("/api/export/library.zip")
	zipBytes := rec.Body.Bytes()
	e.add("Added After Export", "later", nil)
	code, out = e.multi("/api/import/backup", "zip", map[string][]byte{"My Export.zip": zipBytes})
	if code != 200 {
		t.Fatalf("import backup: %v", out)
	}
	name := out["file"].(string)
	if code, out = e.post("/api/restore", `{"file":"`+name+`"}`); code != 200 {
		t.Fatalf("restore of imported zip: %v", out)
	}
	for _, p := range e.s.lib.Products {
		if p.Name == "Added After Export" {
			t.Error("the library should have gone back to the exported state")
		}
	}
	if code, _ = e.multi("/api/import/backup", "zip", map[string][]byte{"bad.zip": []byte("not a zip")}); code == 200 {
		t.Error("a file that is not a library zip must be refused")
	}
}

func TestDiagnostics(t *testing.T) {
	e := newEnv(t)
	e.add("Product", "p", nil)
	_, out := e.get("/api/diagnostics")
	report, _ := out["report"].(string)
	checks, _ := out["checks"].([]any)
	if len(checks) < 6 || !bytes.Contains([]byte(report), []byte("diagnostics")) || !bytes.Contains([]byte(report), []byte(appVersion)) {
		t.Fatalf("diagnostics incomplete: %v", out)
	}
	for _, c := range checks {
		if c.(map[string]any)["status"] == "bad" {
			t.Errorf("a fresh library should not report a failure: %v", c)
		}
	}
	os.Remove(e.s.pdfPath(e.s.lib.Products[len(e.s.lib.Products)-1]))
	_, out = e.get("/api/diagnostics")
	bad := 0
	for _, c := range out["checks"].([]any) {
		if c.(map[string]any)["status"] == "bad" {
			bad++
		}
	}
	if bad == 0 {
		t.Error("a missing PDF must show as a failed check")
	}
}

func TestRecovery(t *testing.T) {
	e := newEnv(t)
	a := e.add("Product A", "aaa", nil)
	e.add("Product B", "bbb", nil)
	// The save before the last one is kept; going back to it keeps the replaced library too.
	if code, out := e.post("/api/recovery/previous-save", ""); code != 200 {
		t.Fatalf("previous save: %v", out)
	}
	if e.s.find(a) == nil {
		t.Fatal("previous save not restored")
	}
	// Product B was added after that save, but its PDF is still in the folder, so it
	// comes back for review instead of being lost.
	var b *Product
	for _, p := range e.s.lib.Products {
		if p.Name == "Product B" {
			b = p
		}
	}
	if b == nil || !b.NeedsReview || e.read(b) != string(pdf("bbb")) {
		t.Fatalf("a PDF left in the folder must not be lost: %+v", b)
	}
	matches, _ := filepath.Glob(e.s.file() + ".replaced-*")
	if len(matches) != 1 {
		t.Error("the library that was replaced must be kept")
	}
	// A damaged previous-save copy is refused and nothing changes.
	_ = os.WriteFile(e.s.file()+".bak", []byte("{broken"), 0o644)
	before := len(e.s.lib.Products)
	if code, _ := e.post("/api/recovery/previous-save", ""); code == 200 || len(e.s.lib.Products) != before {
		t.Error("a damaged copy must not be used")
	}
	// Old versions kept by a replace can be added back as products.
	e.send("/api/products/"+a, map[string]any{"name": "Product A", "category": "Degreasers"}, pdf("aaa v2"))
	_, out := e.get("/api/recovery")
	unlinked := out["unlinked"].([]any)
	if len(unlinked) != 1 {
		t.Fatalf("expected one older version, got %v", unlinked)
	}
	file := unlinked[0].(map[string]any)["file"].(string)
	body, _ := json.Marshal(map[string]string{"file": file})
	if code, out := e.post("/api/recovery/unlinked", string(body)); code != 200 {
		t.Fatalf("add back: %v", out)
	}
	var back *Product
	for _, p := range e.s.lib.Products {
		if p.NeedsReview && p.SHA256 == sumBytes(pdf("aaa")) {
			back = p
		}
	}
	if back == nil || e.read(back) != string(pdf("aaa")) {
		t.Fatal("older version was not added back with its original contents")
	}
	// Rebuild: a PDF dropped in the folder is found, a deleted one is flagged.
	_ = os.WriteFile(filepath.Join(e.s.pdfDir(), "Found-Later.pdf"), pdf("found"), 0o644)
	os.Remove(e.s.pdfPath(back))
	_, out = e.post("/api/recovery/rebuild", "")
	if out["added"].(float64) != 1 || out["missing"].(float64) != 1 {
		t.Errorf("rebuild result: %v", out)
	}
}

func TestPageCountBackfill(t *testing.T) {
	e := newEnv(t)
	id := e.add("Pageless", "p", nil)
	e.post("/api/products/"+id+"/pages", `{"pages":7}`)
	if e.s.find(id).Pages != 7 {
		t.Error("page count not saved")
	}
	if code, _ := e.post("/api/products/"+id+"/pages", `{"pages":-3}`); code == 200 {
		t.Error("nonsense page count accepted")
	}
}

func TestStorageAreas(t *testing.T) {
	e := newEnv(t)
	post := func(body string) (int, map[string]any) { return e.post("/api/locations", body) }
	id := e.add("Sani", "x", map[string]any{"location": "Mop Room"})
	if c, _ := post(`{"locations":["Drystock","Dish Area"]}`); c != 409 {
		t.Fatalf("removing an area that still has a product must be refused, got %d", c)
	}
	if e.s.find(id).Location != "Mop Room" {
		t.Fatal("a refused change touched a product")
	}
	if c, _ := post(`{"locations":["Drystock","Mop Room","Cooler"]}`); c != 200 || len(e.s.lib.Locations) != 3 {
		t.Fatalf("adding areas failed: %d %v", c, e.s.lib.Locations)
	}
	if c, _ := post(`{"locations":["Drystock","Mop Room","Cooler"],"renames":{"Mop Room":"Janitor Closet"}}`); c != 200 {
		t.Fatalf("rename failed: %d", c)
	}
	if e.s.find(id).Location != "Janitor Closet" || e.s.lib.Locations[len(e.s.lib.Locations)-1] != "Janitor Closet" {
		t.Errorf("rename should carry the product across: %q %v", e.s.find(id).Location, e.s.lib.Locations)
	}
	if c, _ := post(`{"locations":["Drystock","Cooler","Janitor Closet","drystock"]}`); c != 200 || len(e.s.lib.Locations) != 3 {
		t.Errorf("duplicates (any case) should collapse: %v", e.s.lib.Locations)
	}
}
