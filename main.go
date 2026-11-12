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
	Category           string `json:"category"`
	Location           string `json:"location"`
	RevisionDate       string `json:"revisionDate"`
	Hazards            string `json:"hazards"`
	FirstAidEyes       string `json:"firstAidEyes"`
	FirstAidSkin       string `json:"firstAidSkin"`
	FirstAidInhalation string `json:"firstAidInhalation"`
	FirstAidSwallowed  string `json:"firstAidSwallowed"`
	PPE                string `json:"ppe"`
	Pages              int    `json:"pages"`
	File               string `json:"file"`    // file name inside the sheets folder
	Missing            bool   `json:"missing"` // set when the PDF is no longer in the folder
	AddedAt            string `json:"addedAt"`
	UpdatedAt          string `json:"updatedAt"`
}

type Library struct {
	Categories []string   `json:"categories"`
	Locations  []string   `json:"locations"`
	Products   []*Product `json:"products"`
	Schema     int        `json:"schema"`
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
	"Dish Room", "Janitorial Closet", "Kitchen Chemical Storage", "Laundry Area", "Maintenance Room",
}

// upgrade brings a library made by an earlier version up to the current
// category list: the three renamed categories carry their products across,
// and the new categories are added without disturbing any the manager made.
func (s *store) upgrade() bool {
	if s.lib.Schema >= 2 {
		return false
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
	s.lib.Schema = 2
	return true
}

type store struct {
	mu  sync.Mutex
	dir string
	lib Library
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
		p.AddedAt, p.UpdatedAt = now(), now()
		s.lib.Products = append(s.lib.Products, p)
	}
	return changed
}

// scan keeps the library in step with the sheets folder: a PDF that someone
// copied into the folder by hand becomes a product, and a product whose PDF
// has gone is flagged. It reports whether the library changed.
func (s *store) scan() bool {
	known := map[string]bool{}
	for _, p := range s.lib.Products {
		known[strings.ToLower(filepath.Base(s.pdfPath(p)))] = true
		_, err := os.Stat(s.pdfPath(p))
		p.Missing = err != nil
	}
	entries, err := os.ReadDir(s.pdfDir())
	if err != nil {
		return false
	}
	changed := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".pdf") || known[strings.ToLower(name)] {
			continue
		}
		title := strings.Join(strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(strings.TrimSuffix(name, filepath.Ext(name)))), " ")
		s.lib.Products = append(s.lib.Products, &Product{
			ID: newID(), Name: title, Category: catchAll, File: name, AddedAt: now(), UpdatedAt: now(),
		})
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
		s.lib = Library{Categories: defaultCategories, Locations: defaultLocations, Products: []*Product{}, Schema: 2}
		s.applyStarter()
		s.scan()
		return s.save()
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &s.lib); err != nil {
		return fmt.Errorf("library.json could not be read (%v). A copy from the previous save is in library.json.bak", err)
	}
	if len(s.lib.Categories) == 0 {
		s.lib.Categories = defaultCategories
	}
	if s.lib.Products == nil {
		s.lib.Products = []*Product{}
	}
	upgraded := s.upgrade()
	starter := s.applyStarter()
	if s.scan() || starter || upgraded {
		return s.save()
	}
	return nil
}

// save writes the library to a temporary file first so a crash or power cut
// can never leave a half-written library behind.
func (s *store) save() error {
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

// retire moves a sheet into the "removed" folder instead of deleting it, so a
// mistaken removal can always be recovered by hand.
func (s *store) retire(p *Product) {
	src := s.pdfPath(p)
	if _, err := os.Stat(src); err != nil {
		return
	}
	name := strings.Trim(unsafeName.ReplaceAllString(p.Name, "-"), "-")
	if len(name) > 60 {
		name = name[:60]
	}
	dst := filepath.Join(s.goneDir(), fmt.Sprintf("%s_%s_%s.pdf", name, time.Now().Format("2006-01-02_150405"), newID()[:6]))
	if err := os.Rename(src, dst); err != nil {
		log.Printf("could not move %s to removed folder: %v", src, err)
	}
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
func (s *store) readUpload(w http.ResponseWriter, r *http.Request) (*Product, string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPDF)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return nil, "", errors.New("the upload could not be read; the file may be too large")
	}
	var in Product
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

func zipDir(src, dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(path, ".tmp") || strings.HasSuffix(path, ".log") {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func now() string { return time.Now().Format("2006-01-02T15:04:05") }

func (s *store) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/library", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.scan() {
			if err := s.save(); err != nil {
				log.Printf("library could not be saved after folder scan: %v", err)
			}
		}
		writeJSON(w, 200, map[string]any{
			"categories": s.lib.Categories,
			"locations":  s.lib.Locations,
			"catchAll":   catchAll,
			"products":   s.lib.Products,
			"dataFolder": s.dir,
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
		in.ID = newID()
		in.AddedAt, in.UpdatedAt = now(), now()
		in.File = s.fileNameFor(in.Name)
		if err := os.Rename(tmp, s.pdfPath(in)); err != nil {
			os.Remove(tmp)
			fail(w, 500, "the PDF could not be saved")
			return
		}
		s.lib.Products = append(s.lib.Products, in)
		if err := s.save(); err != nil {
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, in)
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
		if tmp != "" {
			s.retire(p)
			p.File = s.fileNameFor(in.Name)
			if err := os.Rename(tmp, s.pdfPath(p)); err != nil {
				os.Remove(tmp)
				fail(w, 500, "the new PDF could not be saved")
				return
			}
			p.Pages = in.Pages
		}
		p.Name, p.Manufacturer, p.Category, p.Location = in.Name, in.Manufacturer, in.Category, in.Location
		p.Supplier, p.Tags = in.Supplier, in.Tags
		p.RevisionDate, p.Hazards, p.PPE = in.RevisionDate, in.Hazards, in.PPE
		p.FirstAidEyes, p.FirstAidSkin = in.FirstAidEyes, in.FirstAidSkin
		p.FirstAidInhalation, p.FirstAidSwallowed = in.FirstAidInhalation, in.FirstAidSwallowed
		p.UpdatedAt = now()
		if err := s.save(); err != nil {
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, p)
	})

	mux.HandleFunc("DELETE /api/products/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := r.PathValue("id")
		p := s.find(id)
		if p == nil {
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		s.retire(p)
		kept := s.lib.Products[:0]
		for _, q := range s.lib.Products {
			if q.ID != id {
				kept = append(kept, q)
			}
		}
		s.lib.Products = kept
		if err := s.save(); err != nil {
			fail(w, 500, "the library could not be saved: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
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
		dir := filepath.Join(filepath.Dir(s.dir), "SDS Backups")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fail(w, 500, "the backup folder could not be created")
			return
		}
		name := filepath.Join(dir, "SDS Backup "+time.Now().Format("2006-01-02 1504")+".zip")
		if err := zipDir(s.dir, name); err != nil {
			os.Remove(name)
			fail(w, 500, "the backup could not be written: "+err.Error())
			return
		}
		openFolder(dir)
		writeJSON(w, 200, map[string]string{"file": name})
	})

	mux.HandleFunc("POST /api/open-folder", func(w http.ResponseWriter, r *http.Request) {
		openFolder(s.dir)
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
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 100))
	return strings.TrimSpace(string(b)) == appID
}

func dataDir() string {
	if d := os.Getenv("SDSBOOK_DATA"); d != "" {
		return d
	}
	exe, err := os.Executable()
	if err != nil {
		fatal("The program could not find its own location: " + err.Error())
	}
	return filepath.Join(filepath.Dir(exe), "SDS Data")
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

	s := &store{dir: dataDir()}
	if err := s.load(); err != nil {
		fatal("Muriale's SDS Book could not open its data folder:\n\n" + s.dir + "\n\n" + err.Error() +
			"\n\nMove the program to a folder you can save files in, such as Documents, and try again.")
	}
	if lf, err := os.OpenFile(filepath.Join(s.dir, "sdsbook.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		log.SetOutput(lf)
	}
	sort.SliceStable(s.lib.Products, func(i, j int) bool {
		return strings.ToLower(s.lib.Products[i].Name) < strings.ToLower(s.lib.Products[j].Name)
	})

	mux := http.NewServeMux()
	s.routes(mux)
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		lastPing.Store(time.Now().UnixNano())
		fmt.Fprint(w, appID)
	})
	sub, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	go func() {
		if err := http.Serve(ln, guard(port, mux)); err != nil {
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

	<-quit
}
