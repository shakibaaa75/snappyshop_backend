package otp

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Each script runs atomically inside Redis, so parallel requests cannot get
// around the cooldown or the attempt limit.

// issueScript: KEYS[1]=challenge, KEYS[2]=cooldown marker.
// ARGV: target, hash, ttl_ms, cooldown_ms.  Returns {1,0} or {0,wait_ms}.
var issueScript = redis.NewScript(`
local cd = redis.call('PTTL', KEYS[2])
if cd > 0 then return {0, cd} end
redis.call('DEL', KEYS[1])
redis.call('HSET', KEYS[1], 'target', ARGV[1], 'hash', ARGV[2], 'attempts', 0)
redis.call('PEXPIRE', KEYS[1], ARGV[3])
redis.call('SET', KEYS[2], '1', 'PX', ARGV[4])
return {1, 0}
`)

// verifyScript: KEYS[1]=challenge. ARGV: hash, max_attempts.
// Returns {status, target}: 0 none, 1 ok, 2 locked out, 3 wrong.
var verifyScript = redis.NewScript(`
local h = redis.call('HMGET', KEYS[1], 'target', 'hash')
if not h[1] then return {0, ''} end
local n = redis.call('HINCRBY', KEYS[1], 'attempts', 1)
local max = tonumber(ARGV[2])
if n > max then redis.call('DEL', KEYS[1]); return {2, ''} end
if h[2] == ARGV[1] then redis.call('DEL', KEYS[1]); return {1, h[1]} end
if n >= max then redis.call('DEL', KEYS[1]); return {2, ''} end
return {3, ''}
`)

// hitScript counts one event and returns {count, ttl_ms}. INCR and PEXPIRE
// run in one step so a crash cannot leave a counter that never expires.
var hitScript = redis.NewScript(`
local c = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if c == 1 or ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {c, ttl}
`)

// Redis is a Store backed by Redis, shared by every API instance.
type Redis struct {
	client  *redis.Client
	prefix  string
	timeout time.Duration
}

// NewRedis does not ping: a Redis that is down at startup must not stop the
// API. Requests simply fail (closed) until it is back.
func NewRedis(c *redis.Client, prefix string, opTimeout time.Duration) *Redis {
	if prefix == "" {
		prefix = "shopbd:otp:"
	}
	if opTimeout <= 0 {
		opTimeout = 500 * time.Millisecond
	}
	return &Redis{client: c, prefix: prefix, timeout: opTimeout}
}

// The braces are a Redis Cluster hash tag: both keys of one challenge land on
// the same slot, which multi-key scripts require.
func (r *Redis) keys(p Purpose, subject string) (challenge, cooldown string) {
	tag := "{" + string(p) + ":" + subject + "}"
	return r.prefix + "c:" + tag, r.prefix + "cd:" + tag
}

func (r *Redis) Issue(ctx context.Context, p Purpose, subject, target, hash string, ttl, cooldown time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	// PEXPIRE / SET PX reject 0; a zero cooldown means "none", so use 1 ms.
	if ttl < time.Millisecond {
		ttl = time.Millisecond
	}
	if cooldown < time.Millisecond {
		cooldown = time.Millisecond
	}
	c, cd := r.keys(p, subject)
	res, err := issueScript.Run(ctx, r.client, []string{c, cd},
		target, hash, ttl.Milliseconds(), cooldown.Milliseconds()).Slice()
	if err != nil {
		return err
	}
	if len(res) != 2 {
		return fmt.Errorf("otp: unexpected issue reply %v", res)
	}
	ok, ok1 := res[0].(int64)
	wait, ok2 := res[1].(int64)
	if !ok1 || !ok2 {
		return fmt.Errorf("otp: unexpected issue reply types %T %T", res[0], res[1])
	}
	if ok != 1 {
		return &CooldownError{Wait: time.Duration(wait) * time.Millisecond}
	}
	return nil
}

func (r *Redis) Verify(ctx context.Context, p Purpose, subject, hash string, maxAttempts int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	c, _ := r.keys(p, subject)
	res, err := verifyScript.Run(ctx, r.client, []string{c}, hash, maxAttempts).Slice()
	if err != nil {
		return "", err
	}
	if len(res) != 2 {
		return "", fmt.Errorf("otp: unexpected verify reply %v", res)
	}
	status, ok1 := res[0].(int64)
	target, ok2 := res[1].(string)
	if !ok1 || !ok2 {
		return "", fmt.Errorf("otp: unexpected verify reply types %T %T", res[0], res[1])
	}
	switch status {
	case 1:
		return target, nil
	case 2:
		return "", ErrTooManyAttempts
	case 3:
		return "", ErrInvalid
	default:
		return "", ErrNotFound
	}
}

func (r *Redis) Cancel(ctx context.Context, p Purpose, subject string) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	c, _ := r.keys(p, subject)
	return r.client.Del(ctx, c).Err()
}

func (r *Redis) Hit(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	res, err := hitScript.Run(ctx, r.client, []string{r.prefix + "rl:" + key}, window.Milliseconds()).Slice()
	if err != nil {
		return false, 0, err
	}
	if len(res) != 2 {
		return false, 0, fmt.Errorf("otp: unexpected hit reply %v", res)
	}
	count, ok1 := res[0].(int64)
	ttl, ok2 := res[1].(int64)
	if !ok1 || !ok2 {
		return false, 0, fmt.Errorf("otp: unexpected hit reply types %T %T", res[0], res[1])
	}
	if count > int64(limit) {
		return false, time.Duration(ttl) * time.Millisecond, nil
	}
	return true, 0, nil
}
