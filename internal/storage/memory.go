package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Memory keeps the lake in memory, for tests.
type Memory struct {
	mu    sync.Mutex
	files map[string][]byte
}

// NewMemory returns an empty in-memory lake.
func NewMemory() *Memory { return &Memory{files: map[string][]byte{}} }

func (m *Memory) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for p := range m.files {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *Memory) Exists(_ context.Context, p string) (bool, error) {
	if err := CheckPath(p); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.files[p]
	return ok, nil
}

func (m *Memory) Write(ctx context.Context, p string, r io.Reader) error {
	if err := CheckPath(p); err != nil {
		return err
	}
	b, err := io.ReadAll(readerWithContext{ctx, r}) // buffered first, so readers never see a partial file
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[p] = b
	return nil
}

func (m *Memory) Open(_ context.Context, p string) (io.ReadCloser, error) {
	if err := CheckPath(p); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[p]
	if !ok {
		return nil, fmt.Errorf("%s: %w", p, ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *Memory) Delete(_ context.Context, p string) error {
	if err := CheckPath(p); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, p)
	return nil
}
