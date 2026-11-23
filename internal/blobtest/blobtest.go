// Package blobtest is an in-memory object store for tests.
package blobtest

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/blacmusic/backend/internal/storage"
)

// Mem implements the Blobs interfaces of the upload and worker packages.
type Mem struct {
	mu   sync.Mutex
	data map[string][]byte
}

// New creates an empty store.
func New() *Mem { return &Mem{data: map[string][]byte{}} }

// Put stores bytes directly (simulates the client's PUT).
func (m *Mem) Put(key string, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = b
}

// Get returns an object's bytes.
func (m *Mem) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	return b, ok
}

// Keys lists every stored key.
func (m *Mem) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.data))
	for k := range m.data {
		out = append(out, k)
	}
	return out
}

func (m *Mem) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "http://blobtest.invalid/put/" + key, nil
}

func (m *Mem) Stat(_ context.Context, key string) (int64, error) {
	b, ok := m.Get(key)
	if !ok {
		return 0, storage.ErrNotFound
	}
	return int64(len(b)), nil
}

func (m *Mem) Download(_ context.Context, key, dst string) error {
	b, ok := m.Get(key)
	if !ok {
		return storage.ErrNotFound
	}
	return os.WriteFile(dst, b, 0o600)
}

func (m *Mem) UploadFile(_ context.Context, key, src, _ string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	m.Put(key, b)
	return nil
}

func (m *Mem) Remove(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}
