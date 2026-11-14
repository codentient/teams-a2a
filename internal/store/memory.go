package store

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Memory is an in-process Store. It is correct only for a single replica.
type Memory struct {
	ttl         time.Duration
	maxSessions int
	now         func() time.Time

	mu       sync.Mutex
	sessions map[string]memEntry
	seen     map[string]time.Time
	locks    map[string]memLock
	stop     chan struct{}
	once     sync.Once
}

type memEntry struct {
	data    []byte // stored serialised so callers never share mutable state
	expires time.Time
}

type memLock struct {
	owner   string
	expires time.Time
}

func NewMemory(sessionTTL time.Duration, maxSessions int) *Memory {
	m := &Memory{
		ttl:         sessionTTL,
		maxSessions: maxSessions,
		now:         time.Now,
		sessions:    map[string]memEntry{},
		seen:        map[string]time.Time{},
		locks:       map[string]memLock{},
		stop:        make(chan struct{}),
	}
	go m.janitor()
	return m
}

func (m *Memory) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.sweep()
		}
	}
}

func (m *Memory) sweep() {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, e := range m.sessions {
		if now.After(e.expires) {
			delete(m.sessions, k)
		}
	}
	for k, exp := range m.seen {
		if now.After(exp) {
			delete(m.seen, k)
		}
	}
	for k, l := range m.locks {
		if now.After(l.expires) {
			delete(m.locks, k)
		}
	}
}

func (m *Memory) GetSession(_ context.Context, key string) (*Session, error) {
	m.mu.Lock()
	e, ok := m.sessions[key]
	m.mu.Unlock()
	if !ok || m.now().After(e.expires) {
		return nil, nil
	}
	var s Session
	if err := json.Unmarshal(e.data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *Memory) PutSession(_ context.Context, key string, s *Session) error {
	s.UpdatedAt = m.now()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[key]; !exists && len(m.sessions) >= m.maxSessions {
		m.evictOldestLocked()
	}
	m.sessions[key] = memEntry{data: b, expires: m.now().Add(m.ttl)}
	return nil
}

func (m *Memory) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, e := range m.sessions {
		if oldestKey == "" || e.expires.Before(oldest) {
			oldestKey, oldest = k, e.expires
		}
	}
	delete(m.sessions, oldestKey)
}

func (m *Memory) DeleteSession(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.sessions, key)
	m.mu.Unlock()
	return nil
}

func (m *Memory) FirstDelivery(_ context.Context, id string, ttl time.Duration) (bool, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if exp, ok := m.seen[id]; ok && now.Before(exp) {
		return false, nil
	}
	m.seen[id] = now.Add(ttl)
	return true, nil
}

func (m *Memory) Acquire(_ context.Context, key, owner string, ttl time.Duration) (bool, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.locks[key]; ok && now.Before(l.expires) && l.owner != owner {
		return false, nil
	}
	m.locks[key] = memLock{owner: owner, expires: now.Add(ttl)}
	return true, nil
}

func (m *Memory) Release(_ context.Context, key, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.locks[key]; ok && l.owner == owner {
		delete(m.locks, key)
	}
	return nil
}

func (m *Memory) Ping(context.Context) error { return nil }

func (m *Memory) Close() error {
	m.once.Do(func() { close(m.stop) })
	return nil
}
