package ratelimit

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"shopbd/backend/internal/redisx"
)

// Runs the real Lua script. Skipped unless REDIS_TEST_URL is set, e.g.
//
//	REDIS_TEST_URL=redis://localhost:6379/15 go test ./internal/ratelimit -run Integration
func TestRedisBucketIntegration(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL not set")
	}
	c, err := redisx.Open(redisx.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := NewRedisBucket(c, "test:rl:", time.Second)
	key := fmt.Sprintf("it-%d", time.Now().UnixNano())
	spec := Spec{Capacity: 3, Every: 400 * time.Millisecond}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		ok, _, err := b.Take(ctx, key, spec)
		if err != nil || !ok {
			t.Fatalf("take %d: ok=%v err=%v", i, ok, err)
		}
	}
	ok, retry, err := b.Take(ctx, key, spec)
	if err != nil || ok || retry <= 0 {
		t.Fatalf("4th take: ok=%v retry=%v err=%v", ok, retry, err)
	}
	time.Sleep(450 * time.Millisecond)
	if ok, _, err := b.Take(ctx, key, spec); err != nil || !ok {
		t.Fatalf("after refill: ok=%v err=%v", ok, err)
	}
}
