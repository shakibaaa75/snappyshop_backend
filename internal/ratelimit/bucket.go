package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Spec is a token bucket: it holds up to Capacity tokens and gains one token
// every Every. A request costs one token. Capacity is the allowed burst;
// Capacity/Every is the sustained rate.
type Spec struct {
	Capacity int
	Every    time.Duration
}

func (s Spec) valid() bool { return s.Capacity >= 1 && s.Every > 0 }

// fullRefill is how long an empty bucket takes to fill up again.
func (s Spec) fullRefill() time.Duration { return time.Duration(s.Capacity) * s.Every }

// BucketBackend takes one token from the bucket stored under key.
type BucketBackend interface {
	Take(ctx context.Context, key string, spec Spec) (allowed bool, retryAfter time.Duration, err error)
}

// bucketScript is the whole check-refill-take cycle in one atomic step, so
// concurrent requests on many API instances cannot both take the last token.
// It reads the clock from Redis (TIME), not from the API instance, so clock
// skew between instances cannot grant extra tokens.
//
// KEYS[1] bucket   ARGV: capacity, ms-per-token, cost, ttl-ms
// returns {allowed(0|1), retry_after_ms}
var bucketScript = redis.NewScript(`
local t = redis.call('TIME')
local now = t[1] * 1000 + math.floor(t[2] / 1000)
local cap = tonumber(ARGV[1])
local every = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local h = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(h[1])
local ts = tonumber(h[2])
if tokens == nil or ts == nil then
  tokens = cap
  ts = now
end
local elapsed = now - ts
if elapsed < 0 then elapsed = 0 end
tokens = math.min(cap, tokens + elapsed / every)
local allowed = 0
local retry = 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
else
  retry = math.ceil((cost - tokens) * every)
end
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'ts', now)
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return {allowed, retry}
`)

// RedisBucket is a BucketBackend shared by every API instance.
type RedisBucket struct {
	client  *redis.Client
	prefix  string
	timeout time.Duration
}

// NewRedisBucket does not ping: a Redis that is down at startup must not stop
// the API. The Guard handles errors at request time.
func NewRedisBucket(c *redis.Client, prefix string, opTimeout time.Duration) *RedisBucket {
	if prefix == "" {
		prefix = "shopbd:rl:"
	}
	if opTimeout <= 0 {
		opTimeout = 150 * time.Millisecond
	}
	return &RedisBucket{client: c, prefix: prefix, timeout: opTimeout}
}

func (r *RedisBucket) Take(ctx context.Context, key string, spec Spec) (bool, time.Duration, error) {
	if !spec.valid() {
		return false, 0, errors.New("ratelimit: invalid bucket spec")
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	// Keep the key alive for two full refills: long enough that an idle
	// bucket that is deleted is indistinguishable from a full one.
	ttl := 2 * spec.fullRefill()
	if ttl < time.Minute {
		ttl = time.Minute
	}
	res, err := bucketScript.Run(ctx, r.client, []string{r.prefix + "b:" + key},
		spec.Capacity, spec.Every.Milliseconds(), 1, ttl.Milliseconds()).Slice()
	if err != nil {
		return false, 0, err
	}
	if len(res) != 2 {
		return false, 0, fmt.Errorf("ratelimit: unexpected script reply %v", res)
	}
	allowed, ok1 := res[0].(int64)
	retry, ok2 := res[1].(int64)
	if !ok1 || !ok2 {
		return false, 0, fmt.Errorf("ratelimit: unexpected script reply types %T %T", res[0], res[1])
	}
	return allowed == 1, time.Duration(retry) * time.Millisecond, nil
}
