package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// two programs on one shared folder
func twoOn(t *testing.T) (a, b *store, dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "SDS Data")
	a = &store{dir: dir}
	a.startLock()
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	b = &store{dir: dir}
	b.startLock()
	if err := b.load(); err != nil {
		t.Fatal(err)
	}
	return
}

func srv(s *store) http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	return s.gate(mux)
}

func code(h http.Handler, method, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code
}

func TestSecondCopyIsReadOnlyAndCannotChange(t *testing.T) {
	a, b, dir := twoOn(t)
	if a.isReadOnly() || !b.isReadOnly() {
		t.Fatalf("first should edit, second should view: a=%v b=%v", a.isReadOnly(), b.isReadOnly())
	}
	before, _ := os.ReadFile(filepath.Join(dir, "library.json"))
	hb := srv(b)
	for _, p := range []string{"/api/products", "/api/categories", "/api/binder/confirm", "/api/restore", "/api/notes"} {
		if c := code(hb, "POST", p); c != http.StatusLocked {
			t.Errorf("POST %s on the viewer = %d, want 423", p, c)
		}
	}
	if c := code(hb, "GET", "/api/library"); c != 200 {
		t.Errorf("viewer cannot read the library: %d", c)
	}
	if c := code(hb, "POST", "/api/backup"); c != 200 {
		t.Errorf("viewer should still be able to make a backup: %d", c)
	}
	if err := b.save(); err == nil {
		t.Error("save() must refuse while read-only")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "library.json"))
	if string(before) != string(after) {
		t.Error("the viewer changed library.json")
	}
	if c := code(srv(a), "POST", "/api/notes"); c == http.StatusLocked {
		t.Error("the editor was locked out")
	}
}

func TestAcquireAfterEditorLeaves(t *testing.T) {
	a, b, dir := twoOn(t)
	hb := srv(b)
	if c := code(hb, "POST", "/api/lock/acquire"); c != 409 {
		t.Fatalf("acquire while the editor is live = %d, want 409", c)
	}
	a.unlock(dir) // editor closes the program
	if c := code(hb, "POST", "/api/lock/acquire"); c != 200 {
		t.Fatalf("acquire after the editor left = %d", c)
	}
	if b.isReadOnly() {
		t.Fatal("still read-only after acquiring")
	}
	if c := code(hb, "POST", "/api/notes"); c == http.StatusLocked {
		t.Error("new editor still locked out")
	}
}

func TestStaleLockIsTakenAutomatically(t *testing.T) {
	a, _, dir := twoOn(t)
	li := readLock(dir)
	li.Beat = time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	b := &store{dir: dir}
	// rewrite the lock as an old one
	a.lk.mu.Lock()
	_ = a.writeLockAt(dir, li.Beat)
	a.lk.mu.Unlock()
	b.startLock()
	if b.isReadOnly() {
		t.Fatal("a lock that nobody has refreshed for 10 minutes should be taken over")
	}
}

func TestTakeoverMakesTheOldEditorReadOnly(t *testing.T) {
	a, b, _ := twoOn(t)
	if c := code(srv(b), "POST", "/api/lock/takeover"); c != 200 {
		t.Fatalf("takeover = %d", c)
	}
	a.beat()
	if !a.isReadOnly() {
		t.Fatal("the old editor must notice and become read-only")
	}
	if b.isReadOnly() {
		t.Fatal("the new editor should be editing")
	}
	if c := code(srv(a), "POST", "/api/categories"); c != http.StatusLocked {
		t.Errorf("old editor can still change things: %d", c)
	}
}

func TestStatusNamesTheEditor(t *testing.T) {
	_, b, _ := twoOn(t)
	st := b.lockStatus()
	h, ok := st["holder"].(map[string]any)
	if !ok || h["live"] != true || st["available"] != false {
		t.Fatalf("status = %v", st)
	}
}

func TestCannotMoveLibraryIntoAFolderSomeoneIsEditing(t *testing.T) {
	a, _, _ := twoOn(t)
	other := filepath.Join(t.TempDir(), "Shared")
	c := &store{dir: other}
	c.startLock()
	if err := c.load(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.relocate(other); err == nil {
		t.Fatal("relocating into a library someone else is editing must be refused")
	}
	if a.dir == other {
		t.Fatal("the folder changed anyway")
	}
}

func TestRelocateMovesTheLock(t *testing.T) {
	a, _, dir := twoOn(t)
	target := filepath.Join(t.TempDir(), "New")
	if _, err := a.relocate(target); err != nil {
		t.Fatal(err)
	}
	if readLock(dir) != nil {
		t.Error("the old folder still has our lock")
	}
	if li := readLock(target); li == nil || li.ID != a.lk.id {
		t.Error("the new folder does not have our lock")
	}
}
