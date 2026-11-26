package otp

import (
	"context"
	"sync"
	"time"
)

type memChallenge struct {
	target, hash string
	attempts     int
	expires      time.Time
}

type memCounter struct {
	n       int
	expires time.Time
}

// Memory is a Store that lives inside one process. It exists for unit tests
// and for local development without Redis. It must NOT be used when more than
// one API instance runs: each instance would have its own codes and limits.
type Memory struct {
	mu         sync.Mutex
	now        func() time.Time
	challenges map[string]*memChallenge
	cooldowns  map[string]time.Time
	counters   map[string]*memCounter
}

func NewMemory() *Memory {
	return &Memory{
		now:        time.Now,
		challenges: map[string]*memChallenge{},
		cooldowns:  map[string]time.Time{},
		counters:   map[string]*memCounter{},
	}
}

// WithClock replaces the time source (tests only).
func (m *Memory) WithClock(now func() time.Time) *Memory {
	m.now = now
	return m
}

func memKey(p Purpose, subject string) string { return string(p) + "\x00" + subject }

func (m *Memory) Issue(_ context.Context, p Purpose, subject, target, hash string, ttl, cooldown time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, now := memKey(p, subject), m.now()
	if until, ok := m.cooldowns[k]; ok && until.After(now) {
		return &CooldownError{Wait: until.Sub(now)}
	}
	m.challenges[k] = &memChallenge{target: target, hash: hash, expires: now.Add(ttl)}
	m.cooldowns[k] = now.Add(cooldown)
	return nil
}

func (m *Memory) Verify(_ context.Context, p Purpose, subject, hash string, maxAttempts int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey(p, subject)
	c, ok := m.challenges[k]
	if !ok || !c.expires.After(m.now()) {
		delete(m.challenges, k)
		return "", ErrNotFound
	}
	c.attempts++
	if c.attempts > maxAttempts {
		delete(m.challenges, k)
		return "", ErrTooManyAttempts
	}
	if c.hash == hash {
		delete(m.challenges, k)
		return c.target, nil
	}
	if c.attempts >= maxAttempts {
		delete(m.challenges, k)
		return "", ErrTooManyAttempts
	}
	return "", ErrInvalid
}

func (m *Memory) Cancel(_ context.Context, p Purpose, subject string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.challenges, memKey(p, subject))
	return nil
}

func (m *Memory) Hit(_ context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	c, ok := m.counters[key]
	if !ok || !c.expires.After(now) {
		c = &memCounter{expires: now.Add(window)}
		m.counters[key] = c
	}
	c.n++
	if c.n > limit {
		return false, c.expires.Sub(now), nil
	}
	return true, 0, nil
}
