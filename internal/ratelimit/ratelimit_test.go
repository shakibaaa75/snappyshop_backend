package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestMemoryFixedWindow(t *testing.T) {
	m := NewMemory()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		d, err := m.Allow(ctx, "k", 3, time.Minute)
		if err != nil || !d.Allowed {
			t.Fatalf("request %d should pass: %+v %v", i, d, err)
		}
	}
	d, _ := m.Allow(ctx, "k", 3, time.Minute)
	if d.Allowed || d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Fatalf("4th request should be blocked with a retry time: %+v", d)
	}
	if d, _ := m.Allow(ctx, "other", 3, time.Minute); !d.Allowed {
		t.Fatal("keys must be counted separately")
	}

	now = now.Add(61 * time.Second)
	if d, _ := m.Allow(ctx, "k", 3, time.Minute); !d.Allowed {
		t.Fatal("window should have reset")
	}
}
