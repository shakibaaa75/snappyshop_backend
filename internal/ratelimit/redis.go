package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// windowScript counts one hit and returns {count, ttl_ms} in a single atomic
// step. INCR and PEXPIRE are separate commands, so without a script a crash
// between them would leave a counter that never expires and blocks a key for
// good. The PTTL check repairs a key that somehow lost its expiry.
var windowScript = redis.NewScript(`
local c = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if c == 1 or ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {c, ttl}
`)

// Redis is a Limiter backed by Redis, shared by every API instance.
type Redis struct {
	client  *redis.Client
	prefix  string
	timeout time.Duration
}

// NewRedis connects using a redis:// or rediss:// URL and checks the
// connection, so a bad REDIS_URL stops startup instead of failing later.
func NewRedis(ctx context.Context, url string) (*Redis, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	c := redis.NewClient(opt)
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Ping(pctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Redis{client: c, prefix: "shopbd:rl:", timeout: 250 * time.Millisecond}, nil
}

func (r *Redis) Close() error { return r.client.Close() }

func (r *Redis) Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	res, err := windowScript.Run(ctx, r.client, []string{r.prefix + key}, window.Milliseconds()).Slice()
	if err != nil {
		return Decision{}, err
	}
	if len(res) != 2 {
		return Decision{}, fmt.Errorf("ratelimit: unexpected script reply %v", res)
	}
	count, ok1 := res[0].(int64)
	ttl, ok2 := res[1].(int64)
	if !ok1 || !ok2 {
		return Decision{}, fmt.Errorf("ratelimit: unexpected script reply types %T %T", res[0], res[1])
	}
	if count > int64(limit) {
		return Decision{Allowed: false, RetryAfter: time.Duration(ttl) * time.Millisecond}, nil
	}
	return Decision{Allowed: true}, nil
}
