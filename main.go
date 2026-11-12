// Muriale's SDS Book - a small desktop program for keeping the restaurant's
// Safety Data Sheet library and printing the paper SDS binder.
//
// The program runs a private server that only this computer can reach and
// shows its screens in an application window. All sheets are stored as normal
// PDF files in the "SDS Data" folder next to the program.
package main

import (
	"archive/zip"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed ui
var uiFiles embed.FS

// starter holds the sheets that ship with the program. Each one is copied
// into the library once, the first time a build that contains it is run.
//
//go:embed starter
var starterFiles embed.FS

const (
	appID    = "muriales-sds-book"
	basePort = 47615
	maxPDF   = 150 << 20
)

type Product struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Manufacturer       string `json:"manufacturer"`
	Supplier           string `json:"supplier"` // who it is bought from, e.g. Sysco
	Tags               string `json:"tags"`     // optional extra uses, comma separated
	Aliases            string `json:"aliases"`  // other names it is known by, e.g. the supplier's name for it
	SHA256             string `json:"sha256"`   // fingerprint of the PDF as it was added
	Category           string `json:"category"`
	Location           string `json:"location"`
	RevisionDate       string `json:"revisionDate"`
	Hazards            string `json:"hazards"`
	FirstAidEyes       string `json:"firstAidEyes"`
	FirstAidSkin       string `json:"firstAidSkin"`
	FirstAidInhalation string `json:"firstAidInhalation"`
	FirstAidSwallowed  string `json:"firstAidSwallowed"`
	PPE                string `json:"ppe"`
	// The GHS hazard pictograms and signal word shown in Section 2 of the
	// sheet, as ticked by a manager. They are part of the emergency summary.
	Pictograms   []string `json:"pictograms"`
	SignalWord   string   `json:"signalWord"`
	Pages        int      `json:"pages"`
	File         string   `json:"file"`    // file name inside the sheets folder
	Missing      bool     `json:"missing"` // set when the PDF is no longer in the folder
	PDFUpdatedAt string   `json:"pdfUpdatedAt"`
	// NeedsReview marks a product made from a PDF copied into the folder
	// by hand, until a manager completes its details.
	NeedsReview bool `json:"needsReview"`
	// Who checked the emergency summary against the manufacturer's sheet,
	// when, and which revision of the sheet it was checked against.
	SummaryVerifiedAt     string `json:"summaryVerifiedAt"`
	SummaryVerifiedBy     string `json:"summaryVerifiedBy"`
	SummarySourceRevision string `json:"summarySourceRevision"`
	// SummaryNeedsCheck is set when the PDF is replaced after the summary
	// was verified, so old first-aid text is never trusted against a new sheet.
	SummaryNeedsCheck bool `json:"summaryNeedsCheck"`
	// Set only on removed products, which are kept so they can be restored.
	RemovedAt    string `json:"removedAt,omitempty"`
	ArchivedFile string `json:"archivedFile,omitempty"`
	AddedAt      string `json:"addedAt"`
	UpdatedAt    string `json:"updatedAt"`
}

// BinderItem is what the paper binder held for one product when it was last
// recorded as up to date.
type BinderItem struct {
	Name         string `json:"name"`
	Manufacturer string `json:"manufacturer"`
	Category     string `json:"category"`
	Location     string `json:"location"`
	RevisionDate string `json:"revisionDate"`
	PDFUpdatedAt string `json:"pdfUpdatedAt"`
	SHA256       string `json:"sha256"`
}

// Binder is what the paper binder holds, as confirmed by a manager after
// printing and putting the pages in. UpdatedAt is that confirmation.
type Binder struct {
	UpdatedAt   string                `json:"updatedAt"`
	GeneratedAt string                `json:"generatedAt"`
	Pages       int                   `json:"pages"`
	Items       map[string]BinderItem `json:"items"`
}

// BuildRecord is the last book the program assembled. Assembling a book is
// not proof that it was printed, so it is kept apart from Binder until a
// manager confirms.
type BuildRecord struct {
	At          string                `json:"at"`
	Scope       string                `json:"scope"` // all, selected or index
	Pages       int                   `json:"pages"`
	Complete    bool                  `json:"complete"`
	Missing     []string              `json:"missing"`
	Items       map[string]BinderItem `json:"items"`
	Confirmed   bool                  `json:"confirmed"`
	ConfirmedAt string                `json:"confirmedAt"`
}

type Library struct {
	Categories []string     `json:"categories"`
	Locations  []string     `json:"locations"`
	Products   []*Product   `json:"products"`
	Removed    []*Product   `json:"removed"`
	Notes      []*Note      `json:"notes"`
	Binder     *Binder      `json:"binder"`
	LastBuild  *BuildRecord `json:"lastBuild"`
	Schema     int          `json:"schema"`
	// Starter sheets already copied in, so one that a manager removes
	// does not come back the next time the program opens.
	StarterApplied []string `json:"starterApplied"`
}

// catchAll is the category that always exists, so no product is ever left
// without one.
const catchAll = "Other / Miscellaneous"

var defaultCategories = []string{
	"Dish Machine", "Sanitizers & Disinfectants", "Degreasers", "General Cleaning",
	"Floor Care", "Restroom Cleaning", "Laundry", "Drain Maintenance",
	"Descalers & Delimers", "Pest Control", "Hand Hygiene", "Maintenance Chemicals",
	"Air Care & Deodorizers", catchAll,
}

var defaultLocations = []string{
	"Drystock", "Mop Room", "Dish Area", "Prep Area", "Landing Area", "Barn",
}

// The list shipped before version 1.5.2. A library still holding exactly this list, which no
// manager changed, is moved to the current list; a customised list is left alone.
var oldDefaultLocations = []string{"Dish Room", "Janitorial Closet", "Kitchen Chemical Storage", "Laundry Area", "Maintenance Room"}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

func (s *store) migrateLocations() bool {
	if sameList(s.lib.Locations, oldDefaultLocations) {
		s.lib.Locations = append([]string{}, defaultLocations...)
		return true
	}
	return false
}

// upgrade brings a library made by an earlier version up to the current
// category list: the three renamed categories carry their products across,
// and the new categories are added without disturbing any the manager made.
func (s *store) upgrade() bool {
	if s.lib.Schema >= 3 {
		return false
	}
	for _, p := range s.lib.Products {
		if p.PDFUpdatedAt == "" {
			p.PDFUpdatedAt = p.UpdatedAt
		}
	}
	if s.lib.Schema == 2 {
		s.lib.Schema = 3
		return true
	}
	renamed := map[string]string{"Sanitizers": "Sanitizers & Disinfectants", "Restroom": "Restroom Cleaning", "Other": catchAll}
	have := map[string]bool{}
	var cats []string
	for _, c := range s.lib.Categories {
		if to, ok := renamed[c]; ok {
			c = to
		}
		if c != catchAll && !have[c] {
			have[c] = true
			cats = append(cats, c)
		}
	}
	for _, c := range defaultCategories {
		if c != catchAll && !have[c] {
			cats = append(cats, c)
		}
	}
	s.lib.Categories = append(cats, catchAll)
	for _, p := range s.lib.Products {
		if to, ok := renamed[p.Category]; ok {
			p.Category = to
		}
	}
	if len(s.lib.Locations) == 0 {
		s.lib.Locations = defaultLocations
	}
	s.lib.Schema = 3
	return true
}

type store struct {
	mu  sync.Mutex
	dir string // the library folder
	cfg string // settings file that remembers a library folder chosen by a manager
	lib Library
	hc  map[string]hashEntry // fingerprints already worked out, by file path
	lk  lockState
}

type settings struct {
	DataFolder string `json:"dataFolder"`
}

// relocate points the program at a different library folder, such as a
// OneDrive or network folder. If that folder already holds a library it is
// opened as it is; otherwise the current library is copied there. The old
// folder is never deleted. It reports whether the library was copied.
func (s *store) relocate(target string) (bool, error) {
	target = filepath.Clean(strings.TrimSpace(strings.Trim(strings.TrimSpace(target), `"`)))
	if !filepath.IsAbs(target) {
		return false, errors.New("enter the full path of the folder, for example C:\\Users\\Name\\OneDrive\\SDS Data")
	}
	if strings.EqualFold(target, filepath.Clean(s.dir)) {
		return false, errors.New("the library is already in that folder")
	}
	if rel, err := filepath.Rel(s.dir, target); err == nil && !strings.HasPrefix(rel, "..") {
		return false, errors.New("choose a folder that is not inside the current library folder")
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return false, fmt.Errorf("that folder could not be opened or created: %v", err)
	}
	probe := filepath.Join(target, ".sds-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return false, fmt.Errorf("the program is not allowed to save files in that folder: %v", err)
	}
	os.Remove(probe)

	// Only one person may edit a library at a time, wherever it lives.
	if cur, ok := s.claim(target, false); !ok {
		who := "someone"
		if cur != nil {
			who = cur.who()
		}
		return false, fmt.Errorf("%s is editing the library in that folder. Ask them to close the program first", who)
	}
	committed := false
	defer func() {
		if !committed {
			s.unlock(target)
		}
	}()

	parts := []string{"library.json", "sheets", "removed"}
	copied := false
	cleanup := func() {
		if copied {
			for _, n := range parts {
				_ = os.RemoveAll(filepath.Join(target, n))
			}
		}
	}
	if !exists(filepath.Join(target, "library.json")) {
		for _, n := range parts[1:] {
			if exists(filepath.Join(target, n)) {
				return false, fmt.Errorf("that folder already has a %q folder but no library. Choose an empty folder", n)
			}
		}
		copied = true
		err := copyFile(s.file(), filepath.Join(target, "library.json"))
		for _, n := range parts[1:] {
			if err != nil {
				break
			}
			if err = os.MkdirAll(filepath.Join(target, n), 0o755); err != nil {
				break
			}
			entries, _ := os.ReadDir(filepath.Join(s.dir, n))
			for _, e := range entries {
				if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
					continue
				}
				if err = copyFile(filepath.Join(s.dir, n, e.Name()), filepath.Join(target, n, e.Name())); err != nil {
					break
				}
			}
		}
		if err != nil {
			cleanup()
			return false, fmt.Errorf("the library could not be copied, so nothing was changed: %v", err)
		}
	}
	// Open the library in its new home before committing to it.
	next := &store{dir: target}
	if err := next.load(); err != nil {
		cleanup()
		return false, fmt.Errorf("the library in that folder did not open correctly, so nothing was changed: %v", err)
	}
	if s.cfg != "" {
		b, _ := json.MarshalIndent(settings{DataFolder: target}, "", "  ")
		if err := os.WriteFile(s.cfg, b, 0o644); err != nil {
			cleanup()
			return false, fmt.Errorf("the new location could not be remembered, so nothing was changed: %v", err)
		}
	}
	committed = true
	s.unlock(s.dir)
	s.lk.readOnly.Store(false)
	s.dir, s.lib = target, next.lib
	return copied, nil
}

func (s *store) file() string    { return filepath.Join(s.dir, "library.json") }
func (s *store) pdfDir() string  { return filepath.Join(s.dir, "sheets") }
func (s *store) goneDir() string { return filepath.Join(s.dir, "removed") }
func (s *store) pdfPath(p *Product) string {
	if p.File == "" { // libraries made by version 1.0 named files by id
		return filepath.Join(s.pdfDir(), p.ID+".pdf")
	}
	return filepath.Join(s.pdfDir(), p.File)
}

var badFileChars = regexp.MustCompile(`[^A-Za-z0-9 &()_.,'+-]+`)

// fileNameFor picks a readable, unused file name in the sheets folder, so the
// folder makes sense to a person browsing it in Windows.
func (s *store) fileNameFor(name string) string {
	base := strings.Join(strings.Fields(badFileChars.ReplaceAllString(name, " ")), " ")
	base = strings.Trim(base, " .")
	if len(base) > 80 {
		base = strings.TrimSpace(base[:80])
	}
	if base == "" {
		base = "SDS"
	}
	for i := 1; ; i++ {
		candidate := base + ".pdf"
		if i > 1 {
			candidate = fmt.Sprintf("%s (%d).pdf", base, i)
		}
		if _, err := os.Stat(filepath.Join(s.pdfDir(), candidate)); errors.Is(err, fs.ErrNotExist) {
			return candidate
		}
	}
}

// applyStarter copies in any sheet that ships with the program and has not
// been offered to this library before.
func (s *store) applyStarter() bool {
	b, err := starterFiles.ReadFile("starter/library.json")
	if err != nil {
		return false
	}
	var st Library
	if err := json.Unmarshal(b, &st); err != nil {
		log.Printf("starter library could not be read: %v", err)
		return false
	}
	applied := map[string]bool{}
	for _, id := range s.lib.StarterApplied {
		applied[id] = true
	}
	changed := false
	for _, p := range st.Products {
		if applied[p.ID] {
			continue
		}
		s.lib.StarterApplied = append(s.lib.StarterApplied, p.ID)
		changed = true
		if s.find(p.ID) != nil {
			continue
		}
		pdf, err := starterFiles.ReadFile("starter/sheets/" + p.File)
		if err != nil {
			log.Printf("starter sheet %q is missing from the program: %v", p.File, err)
			continue
		}
		p.File = s.fileNameFor(strings.TrimSuffix(p.File, ".pdf"))
		if err := os.WriteFile(s.pdfPath(p), pdf, 0o644); err != nil {
			log.Printf("starter sheet %q could not be copied: %v", p.File, err)
			continue
		}
		p.AddedAt, p.UpdatedAt, p.PDFUpdatedAt, p.SHA256 = now(), now(), now(), sumBytes(pdf)
		s.lib.Products = append(s.lib.Products, p)
	}
	return changed
}

// scan keeps the library in step with the sheets folder: a PDF that someone
// copied into the folder by hand becomes a product, and a product whose PDF
// has gone is flagged. It reports whether the library changed.
func (s *store) scan() bool {
	known := map[string]bool{}
	baselined := false
	for _, p := range s.lib.Products {
		known[strings.ToLower(filepath.Base(s.pdfPath(p)))] = true
		_, err := os.Stat(s.pdfPath(p))
		p.Missing = err != nil
		if err == nil && p.SHA256 == "" {
			// A library from before fingerprints existed: take them now.
			if sum, herr := hashFile(s.pdfPath(p)); herr == nil {
				p.SHA256, baselined = sum, true
			}
		}
	}
	entries, err := os.ReadDir(s.pdfDir())
	if err != nil {
		return baselined
	}
	changed := baselined
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".pdf") || known[strings.ToLower(name)] {
			continue
		}
		title := titleFromFile(name)
		sum, _ := hashFile(filepath.Join(s.pdfDir(), name))
		np := &Product{
			ID: newID(), Name: title, Category: catchAll, File: name, NeedsReview: true,
			AddedAt: now(), UpdatedAt: now(), PDFUpdatedAt: now(), SHA256: sum,
		}
		// A PDF dropped into the folder arrives with its summary already drafted for a manager to check.
		if d := draftFile(filepath.Join(s.pdfDir(), name)); d != nil {
			if d.Name != "" && regexp.MustCompile(`^[\d\s._-]*$|^sds\b`).MatchString(strings.ToLower(title)) {
				np.Name = d.Name
			}
			applyDraft(np, d)
		}
		s.lib.Products = append(s.lib.Products, np)
		changed = true
	}
	return changed
}

func (s *store) load() error {
	for _, d := range []string{s.dir, s.pdfDir(), s.goneDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	b, err := os.ReadFile(s.file())
	if errors.Is(err, fs.ErrNotExist) {
		s.lib = Library{Categories: defaultCategories, Locations: defaultLocations, Products: []*Product{}, Schema: 3}
		s.applyStarter()
		s.scan()
		if s.isReadOnly() {
			return nil
		}
		return s.save()
	}
	if err != nil {
		return err
	}
	var lib Library
	if err := json.Unmarshal(b, &lib); err != nil {
		return fmt.Errorf("library.json could not be read (%v). A copy from the previous save is in library.json.bak", err)
	}
	s.lib = lib
	if len(s.lib.Categories) == 0 {
		s.lib.Categories = defaultCategories
	}
	if s.lib.Products == nil {
		s.lib.Products = []*Product{}
	}
	upgraded := s.upgrade()
	if s.migrateLocations() {
		upgraded = true
	}
	starter := s.applyStarter()
	if (s.scan() || starter || upgraded) && !s.isReadOnly() {
		return s.save()
	}
	return nil
}

// save writes the library to a temporary file first so a crash or power cut
// can never leave a half-written library behind.
func (s *store) save() error {
	if s.isReadOnly() {
		return errReadOnly
	}
	b, err := json.MarshalIndent(s.lib, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if old, err := os.ReadFile(s.file()); err == nil {
		_ = os.WriteFile(s.file()+".bak", old, 0o644)
	}
	return os.Rename(tmp, s.file())
}

func (s *store) find(id string) *Product {
	for _, p := range s.lib.Products {
		if p.ID == id {
			return p
		}
	}
	return nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9]+`)

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// copyFile copies src to dst and makes sure the copy has reached the disk
// before reporting success.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// archiveName picks an unused, readable file name in the "removed" folder.
func (s *store) archiveName(p *Product) string {
	name := strings.Trim(unsafeName.ReplaceAllString(p.Name, "-"), "-")
	if len(name) > 60 {
		name = name[:60]
	}
	return fmt.Sprintf("%s_%s_%s.pdf", name, time.Now().Format("2006-01-02_150405"), newID()[:6])
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// readUpload parses the form. It returns the product fields and, when a PDF
// was attached, the path of a temporary file holding it.
type upload struct {
	Product
	Verify     bool   `json:"verify"`     // the manager ticked "checked against the SDS"
	VerifiedBy string `json:"verifiedBy"` // their name or initials
}

func (p *Product) summaryText() string {
	return strings.Join([]string{p.Hazards, p.FirstAidEyes, p.FirstAidSkin, p.FirstAidInhalation, p.FirstAidSwallowed, p.PPE,
		p.SignalWord, strings.Join(p.Pictograms, ",")}, "\x00")
}

func (p *Product) hasSummary() bool {
	return strings.TrimSpace(strings.ReplaceAll(p.summaryText(), "\x00", "")) != ""
}

// settleSummary works out the verification state of the emergency summary
// after a save. A summary only counts as verified when a manager says they
// checked it; editing the text or replacing the PDF takes that away.
func settleSummary(p *Product, in *upload, textChanged, pdfReplaced bool) {
	switch {
	case !p.hasSummary():
		p.SummaryVerifiedAt, p.SummaryVerifiedBy, p.SummarySourceRevision, p.SummaryNeedsCheck = "", "", "", false
	case in.Verify:
		p.SummaryVerifiedAt = time.Now().Format("2006-01-02")
		p.SummaryVerifiedBy = strings.TrimSpace(in.VerifiedBy)
		p.SummarySourceRevision = p.RevisionDate
		p.SummaryNeedsCheck = false
	case textChanged:
		p.SummaryVerifiedAt, p.SummaryVerifiedBy, p.SummarySourceRevision, p.SummaryNeedsCheck = "", "", "", false
	case pdfReplaced && p.SummaryVerifiedAt != "":
		p.SummaryNeedsCheck = true
	}
}

func (s *store) readUpload(w http.ResponseWriter, r *http.Request) (*upload, string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPDF)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, "", errors.New("the upload could not be read; the file may be too large")
	}
	var in upload
	if err := json.Unmarshal([]byte(r.FormValue("meta")), &in); err != nil {
		return nil, "", errors.New("the product details could not be read")
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, "", errors.New("a product name is required")
	}
	f, _, err := r.FormFile("pdf")
	if errors.Is(err, http.ErrMissingFile) {
		return &in, "", nil
	}
	if err != nil {
		return nil, "", errors.New("the PDF could not be read")
	}
	defer f.Close()
	head := make([]byte, 1024)
	n, _ := io.ReadFull(f, head)
	if !strings.Contains(string(head[:n]), "%PDF") {
		return nil, "", errors.New("that file is not a PDF")
	}
	tmp, err := os.CreateTemp(s.pdfDir(), "upload-*.tmp")
	if err != nil {
		return nil, "", err
	}
	defer tmp.Close()
	if _, err := tmp.Write(head[:n]); err == nil {
		_, err = io.Copy(tmp, f)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return nil, "", errors.New("the PDF could not be saved")
	}
	return &in, tmp.Name(), nil
}

func snapshot(p *Product) BinderItem {
	return BinderItem{p.Name, p.Manufacturer, p.Category, p.Location, p.RevisionDate, p.PDFUpdatedAt, p.SHA256}
}

func (s *store) backupDir() string { return filepath.Join(filepath.Dir(s.dir), "SDS Backups") }

// backupPath reads the backup file name from the request and makes sure it
// names a zip file inside the backups folder and nothing else.
func (s *store) backupPath(w http.ResponseWriter, r *http.Request) (string, error) {
	var in struct {
		File string `json:"file"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		return "", errors.New("the request could not be read")
	}
	if in.File == "" || in.File != filepath.Base(in.File) || !strings.EqualFold(filepath.Ext(in.File), ".zip") {
		return "", errors.New("choose a backup file from the list")
	}
	path := filepath.Join(s.backupDir(), in.File)
	if !exists(path) {
		return "", errors.New("that backup file is no longer in the backups folder")
	}
	return path, nil
}

type backupInfo struct {
	Products    int      `json:"products"`
	Removed     int      `json:"removed"`
	Sheets      int      `json:"sheets"`
	MissingPDFs []string `json:"missingPdfs"` // products in the backup whose PDF is not in it
}

// backupEntry says whether a file inside a zip belongs to a library backup,
// and returns its cleaned path. Anything else in the zip is ignored.
func backupEntry(name string) (string, bool) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "library.json" {
		return name, true
	}
	for _, dir := range []string{"sheets/", "removed/"} {
		rest := strings.TrimPrefix(name, dir)
		if rest != name && rest != "" && !strings.Contains(rest, "/") && rest != "." && rest != ".." {
			return dir + rest, true
		}
	}
	return "", false
}

func inspectBackup(path string) (*backupInfo, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, errors.New("that file is not a readable backup")
	}
	defer zr.Close()
	var lib *Library
	files := map[string]bool{}
	for _, f := range zr.File {
		name, ok := backupEntry(f.Name)
		if !ok {
			continue
		}
		files[strings.ToLower(name)] = true
		if name == "library.json" {
			rc, err := f.Open()
			if err != nil {
				return nil, errors.New("the backup is damaged")
			}
			b, err := io.ReadAll(io.LimitReader(rc, 64<<20))
			rc.Close()
			lib = &Library{}
			if err != nil || json.Unmarshal(b, lib) != nil {
				return nil, errors.New("the product list inside the backup is damaged")
			}
		}
	}
	if lib == nil {
		return nil, errors.New("that zip file is not an SDS Manager backup (it has no library.json)")
	}
	info := &backupInfo{Products: len(lib.Products), Removed: len(lib.Removed), MissingPDFs: []string{}}
	for name := range files {
		if strings.HasPrefix(name, "sheets/") {
			info.Sheets++
		}
	}
	for _, p := range lib.Products {
		file := p.File
		if file == "" {
			file = p.ID + ".pdf"
		}
		if !files["sheets/"+strings.ToLower(file)] {
			info.MissingPDFs = append(info.MissingPDFs, p.Name)
		}
	}
	return info, nil
}

// restore replaces the library with the contents of a backup. The current
// library is not deleted: it is moved, whole, into a "Before restore" folder
// beside the backups. If anything goes wrong, including the restored library
// failing to open, everything is put back as it was. It returns the folder
// holding the previous library.
func (s *store) restore(zipPath string) (string, error) {
	if _, err := inspectBackup(zipPath); err != nil {
		return "", err
	}
	parts := []string{"library.json", "sheets", "removed"}
	stage := filepath.Join(s.backupDir(), ".restore-in-progress")
	if err := os.RemoveAll(stage); err != nil {
		return "", fmt.Errorf("the restore could not start: %v", err)
	}
	defer os.RemoveAll(stage)
	for _, d := range []string{"sheets", "removed"} {
		if err := os.MkdirAll(filepath.Join(stage, d), 0o755); err != nil {
			return "", fmt.Errorf("the restore could not start: %v", err)
		}
	}

	// 1. Unpack the backup beside the live library, without touching it.
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", errors.New("that file is not a readable backup")
	}
	for _, f := range zr.File {
		name, ok := backupEntry(f.Name)
		if !ok || f.FileInfo().IsDir() {
			continue
		}
		err = func() error {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			out, err := os.Create(filepath.Join(stage, filepath.FromSlash(name)))
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(rc, maxPDF))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			return err
		}()
		if err != nil {
			zr.Close()
			return "", fmt.Errorf("the backup could not be unpacked, so nothing was changed: %v", err)
		}
	}
	zr.Close()

	// 2. Move the current library aside, then move the unpacked one in.
	hold := filepath.Join(s.backupDir(), "Before restore "+time.Now().Format("2006-01-02 150405"))
	if err := os.MkdirAll(hold, 0o755); err != nil {
		return "", fmt.Errorf("the current library could not be set aside, so nothing was changed: %v", err)
	}
	var out, in []string
	undo := func() {
		for _, n := range in {
			_ = os.RemoveAll(filepath.Join(s.dir, n))
		}
		for _, n := range out {
			_ = os.Rename(filepath.Join(hold, n), filepath.Join(s.dir, n))
		}
		_ = os.Remove(hold)
	}
	for _, n := range parts {
		if !exists(filepath.Join(s.dir, n)) {
			continue
		}
		if err := os.Rename(filepath.Join(s.dir, n), filepath.Join(hold, n)); err != nil {
			undo()
			return "", fmt.Errorf("the current library could not be set aside, so nothing was changed: %v", err)
		}
		out = append(out, n)
	}
	for _, n := range parts {
		if !exists(filepath.Join(stage, n)) {
			continue
		}
		if err := os.Rename(filepath.Join(stage, n), filepath.Join(s.dir, n)); err != nil {
			undo()
			return "", fmt.Errorf("the backup could not be put in place, so nothing was changed: %v", err)
		}
		in = append(in, n)
	}

	// 3. Open the restored library exactly as the program does at start-up.
	previous := s.lib
	if err := s.load(); err != nil {
		s.lib = previous
		undo()
		return "", fmt.Errorf("the restored library did not open correctly, so the previous library was put back: %v", err)
	}
	return hold, nil
}

func zipDir(src, dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	err = zipTo(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func now() string { return time.Now().Format("2006-01-02T15:04:05") }

func (s *store) routes(mux *http.ServeMux) {
	s.moreRoutes(mux)
	s.lockRoutes(mux)
	mux.HandleFunc("GET /api/library", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.scan() && !s.isReadOnly() {
			if err := s.save(); err != nil {
				log.Printf("library could not be saved after folder scan: %v", err)
			}
		}
		writeJSON(w, 200, map[string]any{
			"categories": s.lib.Categories,
			"locations":  s.lib.Locations,
			"catchAll":   catchAll,
			"products":   s.lib.Products,
			"removed":    s.lib.Removed,
			"binder":     s.lib.Binder,
			"lastBuild":  s.lib.LastBuild,
			"notes":      s.lib.Notes,
			"version":    appVersion,
			"integrity":  s.integrityProblems(),
			"dataFolder": s.dir,
			"readOnly":   s.isReadOnly(),
		})
	})

	mux.HandleFunc("POST /api/products", func(w http.ResponseWriter, r *http.Request) {
		in, tmp, err := s.readUpload(w, r)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		if tmp == "" {
			fail(w, 400, "choose the SDS PDF for this product")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		p := &in.Product
		p.ID = newID()
		p.AddedAt, p.UpdatedAt, p.PDFUpdatedAt = now(), now(), now()
		p.Missing, p.NeedsReview, p.RemovedAt, p.ArchivedFile, p.SHA256 = false, false, "", "", ""
		p.SummaryVerifiedAt, p.SummaryVerifiedBy, p.SummarySourceRevision, p.SummaryNeedsCheck = "", "", "", false
		settleSummary(p, in, false, false)
		p.File = s.fileNameFor(p.Name)
		if err := os.Rename(tmp, s.pdfPath(p)); err != nil {
			os.Remove(tmp)
			fail(w, 500, "the PDF could not be saved")
			return
		}
		p.SHA256, _ = hashFile(s.pdfPath(p))
		s.lib.Products = append(s.lib.Products, p)
		if err := s.save(); err != nil {
			// Undo, so the folder and the library never disagree.
			s.lib.Products = s.lib.Products[:len(s.lib.Products)-1]
			os.Remove(s.pdfPath(p))
			fail(w, 500, "the library could not be saved, so nothing was added: "+err.Error())
			return
		}
		writeJSON(w, 200, p)
	})

	mux.HandleFunc("POST /api/products/{id}", func(w http.ResponseWriter, r *http.Request) {
		in, tmp, err := s.readUpload(w, r)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		p := s.find(r.PathValue("id"))
		if p == nil {
			if tmp != "" {
				os.Remove(tmp)
			}
			fail(w, 404, "that product is no longer in the library")
			return
		}
		// Replacing a PDF is done so that a failure at any step leaves the
		// product exactly as it was: the old sheet is copied to the "removed"
		// folder first, the new one then takes its place in a single step,
		// and if the library cannot be saved the old sheet is put back.
		before := *p
		replaced := tmp != ""
		var archive, dst string
		if replaced {
			dst = s.pdfPath(p)
			if exists(dst) {
				archive = filepath.Join(s.goneDir(), s.archiveName(p))
				if err := copyFile(dst, archive); err != nil {
					os.Remove(tmp)
					fail(w, 500, "the current PDF could not be archived, so nothing was changed: "+err.Error())
					return
				}
			} else {
				p.File = s.fileNameFor(in.Name)
				dst = s.pdfPath(p)
			}
			if err := os.Rename(tmp, dst); err != nil {
				os.Remove(tmp)
				if archive != "" {
					os.Remove(archive)
				}
				*p = before
				fail(w, 500, "the new PDF could not be put in place, so nothing was changed: "+err.Error())
				return
			}
			p.Pages = in.Pages
			p.PDFUpdatedAt = now()
			p.SHA256, _ = hashFile(dst)
		}
		oldSummary := p.summaryText()
		p.Name, p.Manufacturer, p.Category, p.Location = in.Name, in.Manufacturer, in.Category, in.Location
		p.Supplier, p.Tags, p.Aliases = in.Supplier, in.Tags, in.Aliases
		p.RevisionDate, p.Hazards, p.PPE = in.RevisionDate, in.Hazards, in.PPE
		p.Pictograms, p.SignalWord = in.Pictograms, in.SignalWord
		p.FirstAidEyes, p.FirstAidSkin = in.FirstAidEyes, in.FirstAidSkin
		p.FirstAidInhalation, p.FirstAidSwallowed = in.FirstAidInhalation, in.FirstAidSwallowed
		settleSummary(p, in, p.summaryText() != oldSummary, replaced)
		if p.NeedsReview && strings.TrimSpace(p.Manufacturer) != "" && p.RevisionDate != "" && strings.TrimSpace(p.Location) != "" {
			p.NeedsReview = false
		}
		p.UpdatedAt = now()
		if err := s.save(); err != nil {
			if replaced {
				if archive != "" {
					if cerr := copyFile(archive, dst); cerr == nil {
						os.Remove(archive)
					}
				} else {
					os.Remove(dst)
				}
			}
			*p = before
			fail(w, 500, "the library could not be saved, so nothing was changed: "+err.Error())
			return
		}
		writeJSON(w, 200, p)
	})

	// Removing keeps the product's details and its PDF so that a discontinued
	// product can be looked up or brought back later.
	mux.HandleFunc("DELETE /api/products/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := r.PathValue("id")
		p := s.find(id)
		if p == nil {
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		src, archived := s.pdfPath(p), ""
		if exists(src) {
			archived = s.archiveName(p)
			if err := os.Rename(src, filepath.Join(s.goneDir(), archived)); err != nil {
				fail(w, 500, "the PDF could not be moved to the removed folder, so nothing was changed: "+err.Error())
				return
			}
			_ = os.Chtimes(filepath.Join(s.goneDir(), archived), time.Now(), time.Now()) // the 30 days count from today
		}
		oldProducts, oldRemoved, before := s.lib.Products, s.lib.Removed, *p
		kept := make([]*Product, 0, len(oldProducts))
		for _, q := range oldProducts {
			if q.ID != id {
				kept = append(kept, q)
			}
		}
		p.RemovedAt, p.ArchivedFile, p.File, p.Missing = now(), archived, "", false
		s.lib.Products, s.lib.Removed = kept, append(append([]*Product{}, oldRemoved...), p)
		if err := s.save(); err != nil {
			*p = before
			s.lib.Products, s.lib.Removed = oldProducts, oldRemoved
			if archived != "" {
				_ = os.Rename(filepath.Join(s.goneDir(), archived), src)
			}
			fail(w, 500, "the library could not be saved, so nothing was changed: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/removed/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := r.PathValue("id")
		idx := -1
		for i, q := range s.lib.Removed {
			if q.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			fail(w, 404, "that product is not in the removed list")
			return
		}
		p := s.lib.Removed[idx]
		before := *p
		oldProducts, oldRemoved := s.lib.Products, s.lib.Removed
		src := filepath.Join(s.goneDir(), p.ArchivedFile)
		moved := false
		p.File = ""
		if p.ArchivedFile != "" && exists(src) {
			p.File = s.fileNameFor(p.Name)
			if err := os.Rename(src, s.pdfPath(p)); err != nil {
				*p = before
				fail(w, 500, "the PDF could not be moved back, so nothing was changed: "+err.Error())
				return
			}
			moved = true
		} else {
			p.File = s.fileNameFor(p.Name) // its PDF is gone; the product comes back flagged "PDF missing"
		}
		p.RemovedAt, p.ArchivedFile, p.UpdatedAt = "", "", now()
		rest := append(append([]*Product{}, oldRemoved[:idx]...), oldRemoved[idx+1:]...)
		s.lib.Removed, s.lib.Products = rest, append(append([]*Product{}, oldProducts...), p)
		if err := s.save(); err != nil {
			if moved {
				_ = os.Rename(s.pdfPath(p), src)
			}
			*p = before
			s.lib.Products, s.lib.Removed = oldProducts, oldRemoved
			fail(w, 500, "the library could not be saved, so nothing was changed: "+err.Error())
			return
		}
		writeJSON(w, 200, p)
	})

	mux.HandleFunc("GET /pdf-removed/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		file := ""
		for _, q := range s.lib.Removed {
			if q.ID == r.PathValue("id") {
				file = q.ArchivedFile
			}
		}
		s.mu.Unlock()
		if file == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		http.ServeFile(w, r, filepath.Join(s.goneDir(), filepath.Base(file)))
	})

	// Records that a book was assembled. This is not the same as printing it.
	mux.HandleFunc("POST /api/binder/generated", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Scope    string   `json:"scope"`
			Pages    int      `json:"pages"`
			Complete bool     `json:"complete"`
			Missing  []string `json:"missing"`
			IDs      []string `json:"ids"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			fail(w, 400, "the request could not be read")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rec := &BuildRecord{At: now(), Scope: in.Scope, Pages: in.Pages, Complete: in.Complete && len(in.Missing) == 0, Missing: in.Missing, Items: map[string]BinderItem{}}
		for _, id := range in.IDs {
			if p := s.find(id); p != nil {
				rec.Items[id] = snapshot(p)
			}
		}
		if rec.Scope == "all" && len(rec.Items) != len(s.lib.Products) {
			rec.Scope = "selected" // not every product is in it, so it cannot replace the whole binder
		}
		if rec.Scope != "all" && rec.Scope != "selected" && rec.Scope != "index" {
			fail(w, 400, "unknown kind of book")
			return
		}
		old := s.lib.LastBuild
		s.lib.LastBuild = rec
		if err := s.save(); err != nil {
			s.lib.LastBuild = old
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, rec)
	})

	// A manager confirms that the last complete book was printed and the
	// paper binder updated. Only then does the binder record change.
	mux.HandleFunc("POST /api/binder/confirm", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		lb := s.lib.LastBuild
		if lb == nil || !lb.Complete {
			fail(w, 400, "there is no complete book to confirm. Print the book again first")
			return
		}
		oldBinder, oldBuild := s.lib.Binder, *lb
		nb := &Binder{Items: map[string]BinderItem{}}
		if oldBinder != nil {
			*nb = *oldBinder
			nb.Items = map[string]BinderItem{}
			for k, v := range oldBinder.Items {
				nb.Items[k] = v
			}
		}
		switch lb.Scope {
		case "all":
			nb.Items = map[string]BinderItem{}
			for k, v := range lb.Items {
				nb.Items[k] = v
			}
			nb.Pages = lb.Pages
		case "selected":
			for k, v := range lb.Items {
				nb.Items[k] = v
			}
		case "index":
			if oldBinder == nil {
				fail(w, 400, "print and confirm the entire book first; the index alone cannot start a binder record")
				return
			}
			for k, v := range lb.Items {
				if cur, ok := nb.Items[k]; ok {
					v.PDFUpdatedAt = cur.PDFUpdatedAt
					nb.Items[k] = v
				}
			}
		}
		nb.UpdatedAt, nb.GeneratedAt = now(), lb.At
		lb.Confirmed, lb.ConfirmedAt = true, now()
		s.lib.Binder = nb
		if err := s.save(); err != nil {
			s.lib.Binder, *lb = oldBinder, oldBuild
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, nb)
	})

	// Sheets that were taken out of the paper binder after removal.
	mux.HandleFunc("POST /api/binder/pulled", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.lib.Binder == nil {
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		old := s.lib.Binder
		nb := *old
		nb.Items = map[string]BinderItem{}
		for id, it := range old.Items {
			if s.find(id) != nil {
				nb.Items[id] = it
			}
		}
		nb.UpdatedAt = now()
		s.lib.Binder = &nb
		if err := s.save(); err != nil {
			s.lib.Binder = old
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, nb)
	})

	mux.HandleFunc("GET /api/backups", func(w http.ResponseWriter, r *http.Request) {
		type item struct {
			File    string `json:"file"`
			Created string `json:"created"`
			Size    int64  `json:"size"`
		}
		list := []item{}
		entries, _ := os.ReadDir(s.backupDir())
		for _, e := range entries {
			if info, err := e.Info(); err == nil && !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".zip") {
				list = append(list, item{e.Name(), info.ModTime().Format("2006-01-02T15:04:05"), info.Size()})
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Created > list[j].Created })
		writeJSON(w, 200, map[string]any{"folder": s.backupDir(), "backups": list})
	})

	mux.HandleFunc("POST /api/backups/inspect", func(w http.ResponseWriter, r *http.Request) {
		path, err := s.backupPath(w, r)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		info, err := inspectBackup(path)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, info)
	})

	mux.HandleFunc("POST /api/restore", func(w http.ResponseWriter, r *http.Request) {
		path, err := s.backupPath(w, r)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		kept, err := s.restore(path)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"products": len(s.lib.Products), "previousKeptIn": kept})
	})

	mux.HandleFunc("GET /pdf/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		p := s.find(r.PathValue("id"))
		s.mu.Unlock()
		if p == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		http.ServeFile(w, r, s.pdfPath(p))
	})

	mux.HandleFunc("POST /api/categories", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Categories []string          `json:"categories"`
			Renames    map[string]string `json:"renames"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
			fail(w, 400, "the categories could not be read")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		seen := map[string]bool{}
		var cats []string
		for _, c := range in.Categories {
			c = strings.TrimSpace(c)
			if c != "" && c != catchAll && !seen[strings.ToLower(c)] {
				seen[strings.ToLower(c)] = true
				cats = append(cats, c)
			}
		}
		cats = append(cats, catchAll)
		valid := map[string]bool{}
		for _, c := range cats {
			valid[c] = true
		}
		for _, p := range s.lib.Products {
			if to, ok := in.Renames[p.Category]; ok {
				p.Category = strings.TrimSpace(to)
			}
			if !valid[p.Category] {
				p.Category = catchAll
			}
		}
		s.lib.Categories = cats
		if err := s.save(); err != nil {
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/backup", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		dir := s.backupDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail(w, 500, "the backup folder could not be created")
			return
		}
		name := filepath.Join(dir, "SDS Backup "+time.Now().Format("2006-01-02 150405")+".zip")
		if err := zipDir(s.dir, name); err != nil {
			os.Remove(name)
			fail(w, 500, "the backup could not be written: "+err.Error())
			return
		}
		openFolder(dir)
		writeJSON(w, 200, map[string]string{"file": name})
	})

	mux.HandleFunc("POST /api/data-folder", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			fail(w, 400, "the request could not be read")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		old := s.dir
		copied, err := s.relocate(in.Path)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		log.Printf("library folder changed from %s to %s (copied=%v)", old, s.dir, copied)
		writeJSON(w, 200, map[string]any{"folder": s.dir, "copied": copied, "previous": old, "products": len(s.lib.Products)})
	})

	mux.HandleFunc("POST /api/open-folder", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("which") == "backups" {
			_ = os.MkdirAll(s.backupDir(), 0o755)
			openFolder(s.backupDir())
		} else {
			openFolder(s.dir)
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
}

var lastPing atomic.Int64

// guard only lets this computer's own SDS Book window talk to the server.
func guard(port int, next http.Handler) http.Handler {
	hosts := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !hosts[strings.TrimPrefix(o, "http://")] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func alreadyRunning(port int) bool {
	if os.Getenv("SDSBOOK_SECOND") != "" { // testing: pretend to be a copy on another computer
		return false
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 100))
	return strings.TrimSpace(string(b)) == appID
}

// dataDir works out where the library lives: a folder a manager chose in
// Settings if there is one, otherwise "SDS Data" next to the program. It also
// returns the path of the settings file that remembers the choice.
func dataDir() (dir, cfg string, chosen bool) {
	if d := os.Getenv("SDSBOOK_DATA"); d != "" {
		return d, "", false
	}
	exe, err := os.Executable()
	if err != nil {
		fatal("The program could not find its own location: " + err.Error())
	}
	cfg = filepath.Join(filepath.Dir(exe), "SDS Manager settings.json")
	if b, err := os.ReadFile(cfg); err == nil {
		var st settings
		if json.Unmarshal(b, &st) == nil && st.DataFolder != "" {
			return st.DataFolder, cfg, true
		}
	}
	return filepath.Join(filepath.Dir(exe), "SDS Data"), cfg, false
}

func main() {
	// Find a free port, or hand over to a copy that is already open.
	var ln net.Listener
	port := basePort
	for ; port < basePort+10; port++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			ln = l
			break
		}
		if alreadyRunning(port) {
			_, _ = openWindow(fmt.Sprintf("http://127.0.0.1:%d/", port))
			return
		}
	}
	if ln == nil {
		fatal("Muriale's SDS Book could not start. Restart the computer and try again.")
	}

	dir, cfg, chosen := dataDir()
	// A chosen folder that has gone missing usually means a network or
	// OneDrive folder is offline. Stop here instead of quietly starting an
	// empty library somewhere else.
	if chosen && !exists(filepath.Join(dir, "library.json")) {
		fatal("Muriale's SDS Manager cannot reach its library folder:\n\n" + dir +
			"\n\nCheck that the network drive or OneDrive folder is connected, then open the program again." +
			"\n\nTo go back to the folder next to the program instead, delete the file\n\"SDS Manager settings.json\" beside the program.")
	}
	s := &store{dir: dir, cfg: cfg}
	s.startLock()
	if err := s.load(); err != nil {
		fatal("Muriale's SDS Book could not open its data folder:\n\n" + s.dir + "\n\n" + err.Error() +
			"\n\nMove the program to a folder you can save files in, such as Documents, and try again.")
	}
	if lf, err := os.OpenFile(filepath.Join(s.dir, "sdsbook-"+logName()+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		log.SetOutput(lf)
	}
	sort.SliceStable(s.lib.Products, func(i, j int) bool {
		return strings.ToLower(s.lib.Products[i].Name) < strings.ToLower(s.lib.Products[j].Name)
	})

	s.mu.Lock()
	s.purgeOld()
	s.mu.Unlock()
	go func() {
		for range time.Tick(6 * time.Hour) {
			s.mu.Lock()
			s.purgeOld()
			s.mu.Unlock()
		}
	}()

	mux := http.NewServeMux()
	s.routes(mux)
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		lastPing.Store(time.Now().UnixNano())
		fmt.Fprint(w, appID)
	})
	sub, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	go func() {
		if err := http.Serve(ln, guard(port, s.gate(mux))); err != nil {
			log.Printf("server stopped: %v", err)
		}
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	log.Printf("Muriale's SDS Book running at %s, data in %s", url, s.dir)
	started := time.Now()
	quit := make(chan struct{})

	if win, err := openWindow(url); err != nil {
		log.Printf("could not open a window: %v", err)
	} else if win != nil {
		// When the application window is closed, the program ends.
		go func() {
			_ = win.Wait()
			if time.Since(started) > 8*time.Second {
				close(quit)
			}
		}()
	}

	// Safety net: the window checks in every few seconds. If it has been
	// silent for a few minutes the window is gone, so the program ends.
	go func() {
		for range time.Tick(20 * time.Second) {
			last := lastPing.Load()
			if last == 0 {
				if time.Since(started) > 5*time.Minute {
					close(quit)
					return
				}
				continue
			}
			if time.Since(time.Unix(0, last)) > 3*time.Minute {
				close(quit)
				return
			}
		}
	}()

	go func() {
		for range time.Tick(beatEvery) {
			s.beat()
		}
	}()

	<-quit
	s.mu.Lock()
	finalDir := s.dir
	s.mu.Unlock()
	s.unlock(finalDir)
}

func logName() string {
	h, _ := os.Hostname()
	h = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '_'
	}, h)
	if h == "" {
		h = "computer"
	}
	return h
}

func (s *store) logf(format string, args ...any) { log.Printf(format, args...) }
