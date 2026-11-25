package blob

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Mem is an in-process object store for tests.
type Mem struct {
	mu   sync.Mutex
	objs map[string][]byte
}

// Put stores a copy of data.
func (m *Mem) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objs == nil {
		m.objs = map[string][]byte{}
	}
	m.objs[key] = append([]byte(nil), data...)
	return nil
}

// Get returns a copy of the object.
func (m *Mem) Get(_ context.Context, key string, max int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	if int64(len(b)) > max {
		return nil, ErrTooLarge
	}
	return append([]byte(nil), b...), nil
}

// Delete removes the object. An absent key is not an error.
func (m *Mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

// Has reports whether key is present.
func (m *Mem) Has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objs[key]
	return ok
}

// Keys lists the keys that are present, sorted.
func (m *Mem) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objs))
	for k := range m.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ListKeys returns the keys that start with prefix, sorted.
func (m *Mem) ListKeys(_ context.Context, prefix string) ([]string, error) {
	var out []string
	for _, k := range m.Keys() {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}
