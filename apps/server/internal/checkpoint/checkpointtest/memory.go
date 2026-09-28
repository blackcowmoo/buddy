// Package checkpointtest provides a store shared across simulated job attempts.
package checkpointtest

import (
	"context"
	"sync"
)

// Memory is safe for parallel candidates; its zero value is ready to use.
type Memory struct {
	mu     sync.Mutex
	values map[string]string
}

func (m *Memory) Load(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.values[key]
	return value, ok, nil
}

func (m *Memory) Save(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = make(map[string]string)
	}
	if _, exists := m.values[key]; !exists {
		m.values[key] = value
	}
	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, key)
	return nil
}
