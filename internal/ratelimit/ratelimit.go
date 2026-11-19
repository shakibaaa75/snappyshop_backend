// Package ratelimit holds limiters that are shared between API instances.
// The in-memory limiters in package middleware count per process; with several
// instances behind a load balancer an attacker gets that limit once per
// instance. These count in one place instead.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Decision is the result of counting one request against a limit.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration // how long until the window resets; set when !Allowed
}

// Limiter counts requests per key in a fixed window.
type Limiter interface {
	// Allow counts one request for key and reports whether it is within limit
	// requests per window. A non-nil error means the limiter could not decide.
	Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error)
}

// Memory is a single-process Limiter for tests and local development.
type Memory struct {
	mu   sync.Mutex
	now  func() time.Time
	hits map[string]*memWindow
}

type memWindow struct {
	count int
	reset time.Time
}

func NewMemory() *Memory {
	return &Memory{now: time.Now, hits: map[string]*memWindow{}}
}

func (m *Memory) Allow(_ context.Context, key string, limit int, window time.Duration) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	w, ok := m.hits[key]
	if !ok || !now.Before(w.reset) {
		w = &memWindow{reset: now.Add(window)}
		m.hits[key] = w
	}
	w.count++
	if w.count > limit {
		return Decision{Allowed: false, RetryAfter: w.reset.Sub(now)}, nil
	}
	return Decision{Allowed: true}, nil
}
