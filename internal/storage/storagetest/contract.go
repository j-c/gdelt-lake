// Package storagetest is the contract every storage adapter must meet. Each adapter's tests call Run.
package storagetest

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/j-c/gdelt-lake/internal/storage"
)

// Run checks s against the storage contract. newStorage must return an empty lake each time it's called.
func Run(t *testing.T, newStorage func(t *testing.T) storage.Storage) {
	ctx := context.Background()

	t.Run("write then read back", func(t *testing.T) {
		s := newStorage(t)
		mustWrite(t, s, "gkg/date=2026-10-31/day.parquet", "hello")
		if got := mustRead(t, s, "gkg/date=2026-10-31/day.parquet"); got != "hello" {
			t.Fatalf("read %q", got)
		}
	})

	t.Run("write replaces", func(t *testing.T) {
		s := newStorage(t)
		mustWrite(t, s, "a/b", "first")
		mustWrite(t, s, "a/b", "second")
		if got := mustRead(t, s, "a/b"); got != "second" {
			t.Fatalf("read %q", got)
		}
	})

	t.Run("exists", func(t *testing.T) {
		s := newStorage(t)
		mustWrite(t, s, "a/b", "x")
		for p, want := range map[string]bool{"a/b": true, "a/c": false, "z/b": false} {
			got, err := s.Exists(ctx, p)
			if err != nil || got != want {
				t.Errorf("Exists(%q) = %v, %v; want %v", p, got, err, want)
			}
		}
	})

	t.Run("list by prefix, sorted", func(t *testing.T) {
		s := newStorage(t)
		for _, p := range []string{"gkg/date=2026-10-31/b", "gkg/date=2026-10-31/a", "gkg/date=2026-11-01/a",
			"events/date=2026-10-31/a", "_state/2026-10-31.json"} {
			mustWrite(t, s, p, "x")
		}
		for prefix, want := range map[string][]string{
			"gkg/date=2026-10-31/": {"gkg/date=2026-10-31/a", "gkg/date=2026-10-31/b"},
			"gkg/date=2026-1":      {"gkg/date=2026-10-31/a", "gkg/date=2026-10-31/b", "gkg/date=2026-11-01/a"},
			"_state/":              {"_state/2026-10-31.json"},
			"mentions/":            nil,
			"nothing/here/":        nil,
		} {
			got, err := s.List(ctx, prefix)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("List(%q) = %v, want %v", prefix, got, want)
			}
		}
		all, err := s.List(ctx, "")
		if err != nil || len(all) != 5 {
			t.Errorf("List(\"\") = %v, %v; want all 5 files", all, err)
		}
	})

	t.Run("open missing is ErrNotFound", func(t *testing.T) {
		s := newStorage(t)
		_, err := s.Open(ctx, "no/such/file")
		if !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Open error %v, want ErrNotFound", err)
		}
	})

	t.Run("delete, including a missing file", func(t *testing.T) {
		s := newStorage(t)
		mustWrite(t, s, "a/b", "x")
		if err := s.Delete(ctx, "a/b"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.Exists(ctx, "a/b"); ok {
			t.Fatal("still exists after Delete")
		}
		if err := s.Delete(ctx, "a/b"); err != nil {
			t.Fatalf("deleting a missing file: %v", err)
		}
	})

	t.Run("invalid paths are rejected", func(t *testing.T) {
		s := newStorage(t)
		for _, p := range []string{"", "/abs", "a/../b", "a//b", "a/", "./a", `a\b`, "a/.tmp-x"} {
			if err := s.Write(ctx, p, strings.NewReader("x")); err == nil {
				t.Errorf("Write(%q) succeeded", p)
			}
			if _, err := s.Open(ctx, p); err == nil || errors.Is(err, storage.ErrNotFound) {
				t.Errorf("Open(%q) error %v, want a path error", p, err)
			}
		}
	})

	t.Run("a write in progress is invisible", func(t *testing.T) {
		s := newStorage(t)
		mustWrite(t, s, "a/old", "old")
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- s.Write(ctx, "a/new", pr) }()
		if _, err := pw.Write([]byte("half")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond) // let the adapter get the first half on its way
		if ok, _ := s.Exists(ctx, "a/new"); ok {
			t.Error("a half-written file is visible to Exists")
		}
		if got, _ := s.List(ctx, "a/"); !slices.Equal(got, []string{"a/old"}) {
			t.Errorf("List during a write = %v, want only a/old", got)
		}
		if _, err := pw.Write([]byte(" and the rest")); err != nil {
			t.Fatal(err)
		}
		_ = pw.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := mustRead(t, s, "a/new"); got != "half and the rest" {
			t.Fatalf("read %q", got)
		}
	})

	t.Run("a failed write leaves nothing behind", func(t *testing.T) {
		s := newStorage(t)
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte("partial"))
			_ = pw.CloseWithError(errors.New("download broke"))
		}()
		if err := s.Write(ctx, "a/broken", pr); err == nil {
			t.Fatal("Write succeeded despite a failing reader")
		}
		if got, _ := s.List(ctx, ""); len(got) != 0 {
			t.Fatalf("files left after a failed write: %v", got)
		}
	})

	t.Run("a cancelled write fails", func(t *testing.T) {
		s := newStorage(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.Write(cctx, "a/b", strings.NewReader("x")); err == nil {
			t.Fatal("Write succeeded with a cancelled context")
		}
		if ok, _ := s.Exists(ctx, "a/b"); ok {
			t.Fatal("a cancelled write left a file")
		}
	})
}

func mustWrite(t *testing.T, s storage.Storage, p, body string) {
	t.Helper()
	if err := s.Write(context.Background(), p, strings.NewReader(body)); err != nil {
		t.Fatalf("Write(%q): %v", p, err)
	}
}

func mustRead(t *testing.T, s storage.Storage, p string) string {
	t.Helper()
	r, err := s.Open(context.Background(), p)
	if err != nil {
		t.Fatalf("Open(%q): %v", p, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
