package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Local keeps the lake in a folder. Writes go to a temp file in the target's folder and are renamed into place, so
// they're atomic on any POSIX filesystem.
type Local struct {
	root string
}

// NewLocal uses root as the lake's folder, creating it if needed.
func NewLocal(root string) (*Local, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("lake folder: %w", err)
	}
	return &Local{root: root}, nil
}

func (l *Local) file(p string) string { return filepath.Join(l.root, filepath.FromSlash(p)) }

func (l *Local) List(_ context.Context, prefix string) ([]string, error) {
	// Walk the deepest folder the prefix names, then filter by the full prefix.
	dir := ""
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		dir = prefix[:i]
	}
	start := l.root
	if dir != "" {
		start = l.file(dir)
	}
	var out []string
	err := filepath.WalkDir(start, func(f string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), tempPrefix) {
			return nil
		}
		rel, err := filepath.Rel(l.root, f)
		if err != nil {
			return err
		}
		if p := filepath.ToSlash(rel); strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func (l *Local) Exists(_ context.Context, p string) (bool, error) {
	if err := CheckPath(p); err != nil {
		return false, err
	}
	st, err := os.Stat(l.file(p))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !st.IsDir(), nil
}

func (l *Local) Write(ctx context.Context, p string, r io.Reader) (err error) {
	if err := CheckPath(p); err != nil {
		return err
	}
	dest := l.file(p)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), tempPrefix+path.Base(p)+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, readerWithContext{ctx, r}); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func (l *Local) Open(_ context.Context, p string) (io.ReadCloser, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	f, err := os.Open(l.file(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", p, ErrNotFound)
	}
	return f, err
}

func (l *Local) Delete(_ context.Context, p string) error {
	if err := CheckPath(p); err != nil {
		return err
	}
	err := os.Remove(l.file(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// readerWithContext stops a copy when the context is cancelled.
type readerWithContext struct {
	ctx context.Context
	r   io.Reader
}

func (r readerWithContext) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}
