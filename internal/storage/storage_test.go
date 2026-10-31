package storage_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/j-c/gdelt-lake/internal/storage"
	"github.com/j-c/gdelt-lake/internal/storage/storagetest"
)

func TestLocalContract(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage {
		s, err := storage.NewLocal(filepath.Join(t.TempDir(), "lake"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestMemoryContract(t *testing.T) {
	storagetest.Run(t, func(*testing.T) storage.Storage { return storage.NewMemory() })
}

// A crash mid-write can leave a temp file in the lake folder; List must not report it.
func TestLocalIgnoresLeftoverTempFiles(t *testing.T) {
	root := t.TempDir()
	s, err := storage.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "gkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gkg", ".tmp-day.parquet-123"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(t.Context(), "gkg/")
	if err != nil || len(got) != 0 {
		t.Fatalf("List = %v, %v; want nothing", got, err)
	}
}

func TestCheckPath(t *testing.T) {
	for _, p := range []string{"a", "gkg/date=2026-10-31/20261031T001500-en.parquet", "_state/2026-10-31.json"} {
		if err := storage.CheckPath(p); err != nil {
			t.Errorf("CheckPath(%q) = %v", p, err)
		}
	}
	if err := storage.CheckPath("a/../b"); err == nil || !strings.Contains(err.Error(), "invalid lake path") {
		t.Errorf("CheckPath(a/../b) = %v", err)
	}
}
