package cache

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"shopbd/backend/internal/redisx"
)

// Exercises the real Lua scripts. Skipped unless REDIS_TEST_URL is set.
func TestRedisBackendIntegration(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL not set")
	}
	cl, err := redisx.Open(redisx.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	be := NewRedisBackend(cl, time.Second)
	ctx := context.Background()
	id := fmt.Sprint(time.Now().UnixNano())
	data, ver, lock := "it:d:"+id, "it:v:"+id, "it:l:"+id

	if ok, err := be.SetIfVersion(ctx, data, []byte("a"), time.Minute, ver, "0"); err != nil || !ok {
		t.Fatalf("set with missing version key should use \"0\": ok=%v err=%v", ok, err)
	}
	if err := be.BumpVersion(ctx, ver, time.Hour); err != nil {
		t.Fatal(err)
	}
	if ok, _ := be.SetIfVersion(ctx, data, []byte("b"), time.Minute, ver, "0"); ok {
		t.Fatal("write under an old version must be refused")
	}
	vals, err := be.MGet(ctx, data, ver, "it:missing:"+id)
	if err != nil || string(vals[0]) != "a" || len(vals[1]) == 0 || vals[2] != nil {
		t.Fatalf("mget: %q %v", vals, err)
	}
	if ok, _ := be.TryLock(ctx, lock, "me", time.Minute); !ok {
		t.Fatal("first lock must succeed")
	}
	if ok, _ := be.TryLock(ctx, lock, "you", time.Minute); ok {
		t.Fatal("second lock must fail")
	}
	_ = be.Unlock(ctx, lock, "you") // wrong owner: no effect
	if ok, _ := be.TryLock(ctx, lock, "you", time.Minute); ok {
		t.Fatal("wrong-owner unlock must not release the lock")
	}
	_ = be.Unlock(ctx, lock, "me")
	if ok, _ := be.TryLock(ctx, lock, "you", time.Minute); !ok {
		t.Fatal("lock should be free after the owner released it")
	}
	cl.Del(ctx, data, ver, lock)
}
