package otp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shopbd/backend/internal/redisx"
)

var seq atomic.Int64 // unique names per sub-test, whatever the clock resolution

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestHelpers(t *testing.T) {
	for i := 0; i < 200; i++ {
		c, err := NewCode()
		if err != nil || !IsSixDigits(c) {
			t.Fatalf("NewCode = %q, %v", c, err)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "12345a", " 12345", "١٢٣٤٥٦"} {
		if IsSixDigits(bad) {
			t.Errorf("IsSixDigits(%q) = true", bad)
		}
	}
	key := DeriveKey([]byte("0123456789abcdef0123456789abcdef"))
	base := Hash(key, PurposeEmailChange, "u1", "a@x.com", "123456")
	for name, other := range map[string]string{
		"purpose": Hash(key, PurposePhoneChange, "u1", "a@x.com", "123456"),
		"subject": Hash(key, PurposeEmailChange, "u2", "a@x.com", "123456"),
		"target":  Hash(key, PurposeEmailChange, "u1", "b@x.com", "123456"),
		"code":    Hash(key, PurposeEmailChange, "u1", "a@x.com", "654321"),
		"key":     Hash(DeriveKey([]byte("another-secret-another-secret-123")), PurposeEmailChange, "u1", "a@x.com", "123456"),
	} {
		if other == base {
			t.Errorf("hash does not depend on %s", name)
		}
	}
	if base != Hash(key, PurposeEmailChange, "u1", "a@x.com", "123456") {
		t.Error("hash is not deterministic")
	}
}

// runSuite is shared by the in-memory and the Redis stores, so both must behave
// the same. advance moves time forward (a fake clock, or a real sleep).
func runSuite(t *testing.T, newStore func() Store, advance func(time.Duration), ttl, cooldown time.Duration) {
	ctx := context.Background()
	sub := func() string { return fmt.Sprintf("user-%d", seq.Add(1)) }
	const p = PurposeEmailChange

	t.Run("success is single use and returns the target", func(t *testing.T) {
		s, u := newStore(), sub()
		if err := s.Issue(ctx, p, u, "new@x.com", "h1", ttl, cooldown); err != nil {
			t.Fatal(err)
		}
		got, err := s.Verify(ctx, p, u, "h1", 5)
		if err != nil || got != "new@x.com" {
			t.Fatalf("Verify = %q, %v", got, err)
		}
		if _, err := s.Verify(ctx, p, u, "h1", 5); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second Verify err = %v, want ErrNotFound", err)
		}
	})

	t.Run("wrong codes are counted and lock the code out", func(t *testing.T) {
		s, u := newStore(), sub()
		_ = s.Issue(ctx, p, u, "t", "good", ttl, cooldown)
		if _, err := s.Verify(ctx, p, u, "bad", 3); !errors.Is(err, ErrInvalid) {
			t.Fatalf("1st wrong: %v", err)
		}
		if _, err := s.Verify(ctx, p, u, "bad", 3); !errors.Is(err, ErrInvalid) {
			t.Fatalf("2nd wrong: %v", err)
		}
		if _, err := s.Verify(ctx, p, u, "bad", 3); !errors.Is(err, ErrTooManyAttempts) {
			t.Fatalf("3rd wrong: %v", err)
		}
		// Even the right code is dead now.
		if _, err := s.Verify(ctx, p, u, "good", 3); !errors.Is(err, ErrNotFound) {
			t.Fatalf("right code after lockout: %v", err)
		}
	})

	t.Run("the right code still works on the last allowed attempt", func(t *testing.T) {
		s, u := newStore(), sub()
		_ = s.Issue(ctx, p, u, "t", "good", ttl, cooldown)
		_, _ = s.Verify(ctx, p, u, "bad", 3)
		_, _ = s.Verify(ctx, p, u, "bad", 3)
		if _, err := s.Verify(ctx, p, u, "good", 3); err != nil {
			t.Fatalf("3rd (right) attempt: %v", err)
		}
	})

	t.Run("parallel guesses cannot beat the attempt limit", func(t *testing.T) {
		s, u := newStore(), sub()
		_ = s.Issue(ctx, p, u, "t", "good", ttl, cooldown)
		var wg sync.WaitGroup
		for i := 0; i < 25; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = s.Verify(ctx, p, u, "bad", 5) }()
		}
		wg.Wait()
		if _, err := s.Verify(ctx, p, u, "good", 5); !errors.Is(err, ErrNotFound) {
			t.Fatalf("right code after 25 parallel wrong guesses: %v (must be locked out)", err)
		}
	})

	t.Run("a code expires", func(t *testing.T) {
		s, u := newStore(), sub()
		_ = s.Issue(ctx, p, u, "t", "h", ttl, cooldown)
		advance(ttl + ttl/2)
		if _, err := s.Verify(ctx, p, u, "h", 5); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expired Verify: %v", err)
		}
	})

	t.Run("resend cooldown, then a new code replaces the old one", func(t *testing.T) {
		s, u := newStore(), sub()
		if err := s.Issue(ctx, p, u, "t", "old", ttl, cooldown); err != nil {
			t.Fatal(err)
		}
		var ce *CooldownError
		if err := s.Issue(ctx, p, u, "t", "x", ttl, cooldown); !errors.As(err, &ce) || ce.Wait <= 0 {
			t.Fatalf("immediate re-issue: %v, want *CooldownError with Wait > 0", err)
		}
		// The refused re-issue changed nothing: the old code still verifies.
		if _, err := s.Verify(ctx, p, u, "old", 5); err != nil {
			t.Fatalf("old code after refused re-issue: %v", err)
		}
		advance(cooldown + cooldown/2)
		if err := s.Issue(ctx, p, u, "t2", "first", ttl+cooldown*4, cooldown); err != nil {
			t.Fatalf("issue after cooldown: %v", err)
		}
		advance(cooldown + cooldown/2)
		if err := s.Issue(ctx, p, u, "t3", "second", ttl+cooldown*4, cooldown); err != nil {
			t.Fatalf("re-issue after cooldown: %v", err)
		}
		if _, err := s.Verify(ctx, p, u, "first", 5); !errors.Is(err, ErrInvalid) {
			t.Fatalf("replaced code: %v, want ErrInvalid", err)
		}
		if got, err := s.Verify(ctx, p, u, "second", 5); err != nil || got != "t3" {
			t.Fatalf("new code: %q, %v", got, err)
		}
	})

	t.Run("codes are isolated by user and by purpose", func(t *testing.T) {
		s, u1, u2 := newStore(), sub(), sub()+"-b"
		_ = s.Issue(ctx, p, u1, "t", "h", ttl, cooldown)
		if _, err := s.Verify(ctx, p, u2, "h", 5); !errors.Is(err, ErrNotFound) {
			t.Fatalf("other user: %v", err)
		}
		if _, err := s.Verify(ctx, PurposePhoneChange, u1, "h", 5); !errors.Is(err, ErrNotFound) {
			t.Fatalf("other purpose: %v", err)
		}
		if _, err := s.Verify(ctx, p, u1, "h", 5); err != nil {
			t.Fatalf("owner: %v", err)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		s, u := newStore(), sub()
		_ = s.Issue(ctx, p, u, "t", "h", ttl, cooldown)
		if err := s.Cancel(ctx, p, u); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Verify(ctx, p, u, "h", 5); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after cancel: %v", err)
		}
	})

	t.Run("hit enforces a window limit", func(t *testing.T) {
		s, key := newStore(), "k-"+sub()
		window := ttl
		for i := 1; i <= 3; i++ {
			ok, _, err := s.Hit(ctx, key, 3, window)
			if err != nil || !ok {
				t.Fatalf("hit %d: ok=%v err=%v", i, ok, err)
			}
		}
		ok, retry, err := s.Hit(ctx, key, 3, window)
		if err != nil || ok || retry <= 0 {
			t.Fatalf("4th hit: ok=%v retry=%v err=%v", ok, retry, err)
		}
		advance(window + window/2)
		if ok, _, err := s.Hit(ctx, key, 3, window); err != nil || !ok {
			t.Fatalf("hit after window: ok=%v err=%v", ok, err)
		}
	})
}

func TestMemoryStore(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	// One shared store per sub-test is created by newStore; all share the clock.
	runSuite(t, func() Store { return NewMemory().WithClock(clock.Now) }, clock.Advance, 10*time.Minute, time.Minute)
}

// Runs the real Lua scripts. Skipped unless REDIS_TEST_URL is set, e.g.
//
//	REDIS_TEST_URL=redis://localhost:6379/15 go test ./internal/otp -run Redis
func TestRedisStoreIntegration(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL not set")
	}
	c, err := redisx.Open(redisx.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Real time passes here, so TTL and cooldown are short.
	runSuite(t,
		func() Store {
			return NewRedis(c, fmt.Sprintf("test:otp:%d:%d:", time.Now().UnixNano(), seq.Add(1)), time.Second)
		},
		time.Sleep, 600*time.Millisecond, 300*time.Millisecond)
}
