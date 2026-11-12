package main

// Automatic safety copies, so a mistake with the library folder (copying a
// folder over it, an overwrite, a wrong click) can always be undone.
//
//   - Once a day, the first time the library is saved, the library as it was
//     before that save is copied to <backups>/Automatic/library-YYYY-MM-DD.json.
//   - Whenever a save would shrink a library of five or more products to less
//     than half its size, the old library is kept as library-before-big-change.
//   - Once a week, when the program starts, the whole library folder (list and
//     PDFs) is zipped into the backups folder as "Automatic Backup <date>.zip".
//
// They sit in the backups folder, beside the library folder rather than inside
// it, so replacing the library folder does not take them away. The newest few
// of each are kept.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	keepDailyCopies = 30
	keepBigChange   = 10
	keepWeeklyZips  = 6
)

func (s *store) autoDir() string { return filepath.Join(s.backupDir(), "Automatic") }

// prune keeps the newest `keep` files in dir whose names start with prefix.
func prune(dir, prefix string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // dated names sort oldest first
	for len(names) > keep {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// safetyCopy runs just before the library is saved. The library file on disk
// is still the old one at this point. It never stops a save from happening.
func (s *store) safetyCopy(next []byte) {
	old, err := os.ReadFile(s.file())
	if err != nil || len(old) == 0 {
		return
	}
	dir := s.autoDir()
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	daily := filepath.Join(dir, "library-"+time.Now().Format("2006-01-02")+".json")
	if _, err := os.Stat(daily); err != nil {
		if os.WriteFile(daily, old, 0o644) == nil {
			prune(dir, "library-2", keepDailyCopies)
		}
	}
	var was, will struct {
		Products []json.RawMessage `json:"products"`
	}
	if json.Unmarshal(old, &was) == nil && json.Unmarshal(next, &will) == nil && len(was.Products) >= 5 && len(will.Products)*2 < len(was.Products) {
		name := filepath.Join(dir, "library-before-big-change-"+time.Now().Format("2006-01-02-150405")+".json")
		if os.WriteFile(name, old, 0o644) == nil {
			prune(dir, "library-before-big-change-", keepBigChange)
		}
	}
}

// weeklyZip makes a full automatic backup if there has not been one for a week.
func (s *store) weeklyZip() {
	dir := s.backupDir()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "Automatic Backup ") {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) < 7*24*time.Hour {
				return
			}
		}
	}
	if !exists(s.file()) || os.MkdirAll(dir, 0o755) != nil {
		return
	}
	name := filepath.Join(dir, "Automatic Backup "+time.Now().Format("2006-01-02")+".zip")
	if zipDir(s.dir, name) != nil {
		_ = os.Remove(name)
		return
	}
	prune(dir, "Automatic Backup ", keepWeeklyZips)
}
