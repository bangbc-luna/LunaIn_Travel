// Package storage abstracts object storage (S3-compatible in production).
package storage

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// Storage stores objects and hands out time-limited download URLs.
type Storage interface {
	// Put uploads body (size bytes) under key.
	Put(ctx context.Context, key, contentType string, body io.Reader, size int64) error
	// PresignGet returns a download URL for key that expires after ttl.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// Memory is an in-process Storage for tests.
type Memory struct {
	mu      sync.Mutex
	objects map[string][]byte
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory { return &Memory{objects: map[string][]byte{}} }

// Put stores the whole body in memory.
func (m *Memory) Put(_ context.Context, key, _ string, body io.Reader, _ int64) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("memory put: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	return nil
}

// PresignGet returns "memory://<key>?ttl=<ttl>".
func (m *Memory) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	return "memory://" + key + "?ttl=" + ttl.String(), nil
}

// Has reports whether key exists.
func (m *Memory) Has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

// Len returns the number of stored objects.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}
