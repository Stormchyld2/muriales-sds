package main

// The "someone is editing" lock. Several people can run the program against one
// shared library folder, but only one of them may change it at a time. The one
// who is editing keeps a small file, edit.lock, in the library folder and
// refreshes it every few seconds. Everyone else opens the library read-only:
// they can search, view and print, and every change is refused until the editor
// closes the program (or the lock goes stale because the computer died).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	lockStale = 3 * time.Minute  // a lock nobody has refreshed for this long is abandoned
	beatEvery = 20 * time.Second // how often the editor refreshes its lock
)

type lockInfo struct {
	ID    string `json:"id"`
	User  string `json:"user"`
	Host  string `json:"host"`
	Since string `json:"since"`
	Beat  string `json:"beat"` // UTC, RFC 3339
}

type lockState struct {
	mu       sync.Mutex
	id       string
	since    string
	readOnly atomic.Bool
}

func lockPath(dir string) string { return filepath.Join(dir, "edit.lock") }

func readLock(dir string) *lockInfo {
	b, err := os.ReadFile(lockPath(dir))
	if err != nil {
		return nil
	}
	var li lockInfo
	if json.Unmarshal(b, &li) != nil || li.ID == "" {
		return nil // unreadable: treated as no lock
	}
	return &li
}

func (li *lockInfo) live() bool {
	t, err := time.Parse(time.RFC3339, li.Beat)
	return err == nil && time.Since(t) < lockStale
}

func (li *lockInfo) who() string {
	switch {
	case li.User != "" && li.Host != "":
		return li.User + " on " + li.Host
	case li.Host != "":
		return li.Host
	case li.User != "":
		return li.User
	}
	return "Someone"
}

func me() (user, host string) {
	user = os.Getenv("USERNAME")
	if user == "" {
		user = os.Getenv("USER")
	}
	host, _ = os.Hostname()
	return
}

func (s *store) writeLock(dir string) error {
	return s.writeLockAt(dir, time.Now().UTC().Format(time.RFC3339))
}

func (s *store) writeLockAt(dir, beat string) error {
	user, host := me()
	if s.lk.since == "" {
		s.lk.since = now()
	}
	b, _ := json.Marshal(lockInfo{ID: s.lk.id, User: user, Host: host, Since: s.lk.since, Beat: beat})
	tmp := lockPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, lockPath(dir))
}

// claim tries to become the editor of the library in dir. It returns the
// current holder when someone else has it. force takes it over regardless.
func (s *store) claim(dir string, force bool) (*lockInfo, bool) {
	s.lk.mu.Lock()
	defer s.lk.mu.Unlock()
	if s.lk.id == "" {
		s.lk.id = newID()
	}
	if cur := readLock(dir); cur != nil && cur.ID != s.lk.id && cur.live() && !force && !sameComputer(cur) {
		return cur, false
	}
	_ = os.MkdirAll(dir, 0o755)
	if err := s.writeLock(dir); err != nil {
		// A folder that cannot be written to cannot be edited either.
		return &lockInfo{User: "the program", Host: "(this folder cannot be written to: " + err.Error() + ")"}, false
	}
	time.Sleep(150 * time.Millisecond) // two programs starting together: the later write wins
	if cur := readLock(dir); cur == nil || cur.ID != s.lk.id {
		return cur, false
	}
	return nil, true
}

// start is called once at start-up, before the library is read.
func (s *store) startLock() {
	_, ok := s.claim(s.dir, false)
	s.lk.readOnly.Store(!ok)
}

// beat refreshes the lock, or notices that it was taken over.
func (s *store) beat() {
	s.mu.Lock()
	dir := s.dir
	s.mu.Unlock()
	if s.lk.readOnly.Load() {
		return
	}
	s.lk.mu.Lock()
	defer s.lk.mu.Unlock()
	cur := readLock(dir)
	if cur != nil && cur.ID != s.lk.id {
		s.lk.readOnly.Store(true)
		s.logf("the edit lock was taken over by %s; this copy is now read-only", cur.who())
		return
	}
	_ = s.writeLock(dir)
}

// unlock gives up the lock when the program closes.
func (s *store) unlock(dir string) {
	s.lk.mu.Lock()
	defer s.lk.mu.Unlock()
	if cur := readLock(dir); cur != nil && cur.ID == s.lk.id {
		_ = os.Remove(lockPath(dir))
	}
}

func (s *store) isReadOnly() bool { return s.lk.readOnly.Load() }

var errReadOnly = errors.New("the library is open read-only because someone else is editing it")

func (s *store) lockStatus() map[string]any {
	s.mu.Lock()
	dir := s.dir
	s.mu.Unlock()
	user, host := me()
	out := map[string]any{"readOnly": s.isReadOnly(), "you": user + " on " + host, "available": true}
	cur := readLock(dir)
	if cur != nil && cur.ID != s.lk.id {
		live := cur.live()
		out["holder"] = map[string]any{"who": cur.who(), "since": cur.Since, "beat": cur.Beat, "live": live}
		out["available"] = !live
	}
	return out
}

func (s *store) holderText() string {
	s.mu.Lock()
	dir := s.dir
	s.mu.Unlock()
	if cur := readLock(dir); cur != nil && cur.ID != s.lk.id {
		return cur.who()
	}
	return "Another person"
}

// readOnlyOK lists the changes that stay possible without the lock: none of them alter the library.
var readOnlyOK = map[string]bool{
	"/api/data-folder": true, "/api/backup": true, "/api/backups/inspect": true,
	"/api/open-folder": true, "/api/import/backup": true,
}

// gate refuses every change to the library while this copy is read-only.
func (s *store) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || !s.isReadOnly() ||
			readOnlyOK[r.URL.Path] || strings.HasPrefix(r.URL.Path, "/api/lock") {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusLocked, map[string]any{"readOnly": true,
			"error": s.holderText() + " is editing the library, so changes are turned off on this computer. You can still search, view and print. Nothing was changed."})
	})
}

func (s *store) lockRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/lock", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.lockStatus())
	})
	take := func(force bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if !s.isReadOnly() {
				writeJSON(w, 200, map[string]bool{"ok": true})
				return
			}
			if cur, ok := s.claim(s.dir, force); !ok {
				who := "someone else"
				if cur != nil {
					who = cur.who()
				}
				fail(w, 409, who+" is still editing the library. Ask them to close the program, or wait a few minutes.")
				return
			}
			// Read the library again: it may have changed while this copy was only viewing.
			s.lk.readOnly.Store(false)
			s.hc = nil
			if err := s.load(); err != nil {
				s.lk.readOnly.Store(true)
				s.unlockLocked(s.dir)
				fail(w, 500, "the library could not be read: "+err.Error())
				return
			}
			s.logf("this copy took over editing (force=%v)", force)
			writeJSON(w, 200, map[string]bool{"ok": true})
		}
	}
	mux.HandleFunc("POST /api/lock/acquire", take(false))
	mux.HandleFunc("POST /api/lock/takeover", take(true))
}

// unlockLocked is unlock for callers that already hold s.mu.
func (s *store) unlockLocked(dir string) { s.unlock(dir) }

var _ = fmt.Sprintf

// sameComputer reports whether a lock was left by this very computer and user. The program
// cannot be open twice on one computer, so such a lock is left over from a crash or a
// forced close and is taken back at once instead of making the manager wait.
func sameComputer(cur *lockInfo) bool {
	if os.Getenv("SDSBOOK_SECOND") != "" { // testing: pretend to be a copy on another computer
		return false
	}
	u, h := me()
	return h != "" && cur.Host == h && cur.User == u
}
