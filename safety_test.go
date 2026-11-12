package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafetyCopy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "SDS Data")
	os.MkdirAll(dir, 0o755)
	s := &store{dir: dir}
	old := `{"products":[{},{},{},{},{},{}]}`
	os.WriteFile(s.file(), []byte(old), 0o644)
	s.safetyCopy([]byte(`{"products":[{}]}`))
	es, _ := os.ReadDir(s.autoDir())
	var daily, big int
	for _, e := range es {
		if strings.Contains(e.Name(), "big-change") {
			big++
		} else {
			daily++
		}
	}
	if daily != 1 || big != 1 {
		t.Fatalf("daily=%d big=%d", daily, big)
	}
	s.safetyCopy([]byte(old))
	es, _ = os.ReadDir(s.autoDir())
	if len(es) != 3 && len(es) != 2 {
		t.Fatalf("unexpected %d", len(es))
	}
}
