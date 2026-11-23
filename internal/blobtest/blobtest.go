// Package blobtest is an in-memory object store for tests.
package blobtest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/blacmusic/backend/internal/storage"
)

// Mem implements the Blobs interfaces of the upload and worker packages.
type Mem struct {
	mu      sync.Mutex
	data    map[string][]byte
	uploads map[string]*multipart // open multipart uploads by upload id
	aborted []string
	nextID  int
}

type multipart struct {
	key   string
	parts map[int][]byte
}

// New creates an empty store.
func New() *Mem { return &Mem{data: map[string][]byte{}, uploads: map[string]*multipart{}} }

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

func (m *Mem) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	b, ok := m.Get(key)
	if !ok {
		return nil, 0, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (m *Mem) StartMultipart(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("mp-%d", m.nextID)
	m.uploads[id] = &multipart{key: key, parts: map[int][]byte{}}
	return id, nil
}

func (m *Mem) PresignPart(_ context.Context, key, uploadID string, number int, _ time.Duration) (string, error) {
	return fmt.Sprintf("http://blobtest.invalid/part/%s?uploadId=%s&partNumber=%d", key, uploadID, number), nil
}

// PutPart simulates the client's PUT of one part.
func (m *Mem) PutPart(uploadID string, number int, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mp, ok := m.uploads[uploadID]; ok {
		mp.parts[number] = data
	}
}

func (m *Mem) ListParts(_ context.Context, _, uploadID string) ([]storage.Part, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mp, ok := m.uploads[uploadID]
	if !ok {
		return nil, storage.ErrNotFound
	}
	out := make([]storage.Part, 0, len(mp.parts))
	for n, b := range mp.parts {
		out = append(out, storage.Part{Number: n, Size: int64(len(b)), ETag: fmt.Sprintf("etag-%d", n)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (m *Mem) CompleteMultipart(_ context.Context, key, uploadID string, parts []storage.Part) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mp, ok := m.uploads[uploadID]
	if !ok {
		return storage.ErrNotFound
	}
	var all []byte
	for _, p := range parts {
		b, ok := mp.parts[p.Number]
		if !ok {
			return fmt.Errorf("part %d missing", p.Number)
		}
		all = append(all, b...)
	}
	m.data[key] = all
	delete(m.uploads, uploadID)
	return nil
}

func (m *Mem) AbortMultipart(_ context.Context, _, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.uploads, uploadID)
	m.aborted = append(m.aborted, uploadID)
	return nil
}

// Aborted lists the multipart uploads that were aborted.
func (m *Mem) Aborted() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.aborted...)
}

// OpenUploads returns the number of multipart uploads still open.
func (m *Mem) OpenUploads() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.uploads)
}
