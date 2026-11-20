package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Backend is the small set of Redis operations the cache needs. Keeping it an
// interface lets tests inject latency, failures and crashes without a server.
type Backend interface {
	// MGet returns one element per key; a missing key is a nil slice.
	MGet(ctx context.Context, keys ...string) ([][]byte, error)
	// SetIfVersion stores val under key (with ttl) only if the version key
	// still holds expectVer ("0" when it does not exist). It reports whether
	// the value was stored. This is what stops a slow loader from writing
	// data that an update has already made stale.
	SetIfVersion(ctx context.Context, key string, val []byte, ttl time.Duration, verKey, expectVer string) (bool, error)
	// BumpVersion replaces the version with a new, larger value; every entry
	// written under the old version becomes invalid at once.
	BumpVersion(ctx context.Context, verKey string, keepFor time.Duration) error
	// TryLock sets key=token only if it does not exist (SET NX PX).
	TryLock(ctx context.Context, key, token string, ttl time.Duration) (bool, error)
	// Unlock deletes key only if it still holds token.
	Unlock(ctx context.Context, key, token string) error
}

var setIfVersionScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[2])
if not cur then cur = '0' end
if cur ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
return 1
`)

// The new version is the Redis clock in microseconds, forced to be larger
// than the current one, so versions never repeat even after a restart of the
// API. Values stay below 2^53 and are exact in Lua numbers.
var bumpScript = redis.NewScript(`
local t = redis.call('TIME')
local v = t[1] * 1000000 + t[2]
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
if cur >= v then v = cur + 1 end
local s = string.format('%d', v)
redis.call('SET', KEYS[1], s, 'PX', ARGV[1])
return s
`)

var unlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// RedisBackend implements Backend on a go-redis client.
type RedisBackend struct {
	c       *redis.Client
	timeout time.Duration
}

func NewRedisBackend(c *redis.Client, opTimeout time.Duration) *RedisBackend {
	if opTimeout <= 0 {
		opTimeout = 150 * time.Millisecond
	}
	return &RedisBackend{c: c, timeout: opTimeout}
}

func (r *RedisBackend) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, r.timeout)
}

func (r *RedisBackend) MGet(ctx context.Context, keys ...string) ([][]byte, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	vals, err := r.c.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(vals))
	for i, v := range vals {
		if s, ok := v.(string); ok {
			out[i] = []byte(s)
		}
	}
	return out, nil
}

func (r *RedisBackend) SetIfVersion(ctx context.Context, key string, val []byte, ttl time.Duration, verKey, expectVer string) (bool, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	n, err := setIfVersionScript.Run(ctx, r.c, []string{key, verKey}, expectVer, val, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (r *RedisBackend) BumpVersion(ctx context.Context, verKey string, keepFor time.Duration) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	return bumpScript.Run(ctx, r.c, []string{verKey}, keepFor.Milliseconds()).Err()
}

func (r *RedisBackend) TryLock(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	return r.c.SetNX(ctx, key, token, ttl).Result()
}

func (r *RedisBackend) Unlock(ctx context.Context, key, token string) error {
	ctx, cancel := r.ctx(ctx)
	defer cancel()
	err := unlockScript.Run(ctx, r.c, []string{key}, token).Err()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	return err
}
