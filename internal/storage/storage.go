// Package storage is where the lake lives: a small interface with one adapter per backend. Paths are relative,
// slash-separated keys such as "gkg/date=2026-10-31/day.parquet", the same on every backend.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNotFound is returned by Open for a path that doesn't exist.
var ErrNotFound = errors.New("not found")

// Storage holds the lake's files. Every adapter passes the contract tests in storagetest.
type Storage interface {
	// List returns every path starting with prefix (a plain string prefix, as in object stores), sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	// Exists reports whether path exists.
	Exists(ctx context.Context, path string) (bool, error)
	// Write stores everything read from r at path, replacing any existing file. It's atomic: until Write returns,
	// readers see either the old file or nothing, never part of the new one.
	Write(ctx context.Context, path string, r io.Reader) error
	// Open reads the file at path; ErrNotFound if there is none.
	Open(ctx context.Context, path string) (io.ReadCloser, error)
	// Delete removes path. Deleting a path that doesn't exist is not an error.
	Delete(ctx context.Context, path string) error
}

// tempPrefix marks a file being written by the local adapter. No lake path may use it.
const tempPrefix = ".tmp-"

// CheckPath rejects paths that aren't plain relative keys: empty, absolute, with empty, "." or ".." segments,
// backslashes, or a segment starting with the temp-file prefix.
func CheckPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, `\`) {
		return fmt.Errorf("invalid lake path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, tempPrefix) {
			return fmt.Errorf("invalid lake path %q", p)
		}
	}
	return nil
}
