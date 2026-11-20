// Package cache is a read-through cache on Redis with cache-stampede
// protection. MongoDB stays the source of truth; Redis only holds copies.
//
// A request for a key goes through these layers, in order:
//
//  1. Redis read (one MGET: value + version tag). A hit returns at once.
//  2. singleflight: identical misses inside one API instance collapse into a
//     single flight. Waiters block on a channel (no busy-waiting) and each
//     can give up through its own context.
//  3. Redis lock (SET NX PX, unique token, TTL): one instance across the whole
//     fleet becomes the loader. Others poll Redis with bounded, jittered
//     exponential backoff. If the loader crashes, the lock expires and a
//     waiter takes over.
//  4. Double check: after getting the lock, read Redis again, because another
//     loader may have finished between our miss and our lock.
//  5. Load from MongoDB through a bounded gate (max concurrent loads per
//     instance); excess is refused with ErrBusy instead of queued.
//  6. Write back only if the key's version tag is unchanged since step 4
//     (Lua compare-and-set). An update that bumps the tag in the meantime
//     makes the write a no-op, so stale data is never stored.
//
// Redis failures never become a flood of database queries: the circuit
// breaker skips Redis, singleflight still collapses identical keys, the gate
// still caps concurrent loads, and a tiny bounded in-process cache absorbs
// repeats for a few seconds.
//
// NEVER use this cache for checkout prices or stock. Checkout reads MongoDB.
package cache

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"shopbd/backend/internal/redisx"
)

var (
	// ErrNotFound is returned by loaders for a legitimately missing item. The
	// miss is cached briefly (Namespace.NegativeTTL) so repeated requests for
	// a nonexistent product do not reach MongoDB.
	ErrNotFound = errors.New("cache: not found")
	// ErrBusy means the instance is at its limit of waiting requests or
	// concurrent database loads. Answer 503 with Retry-After.
	ErrBusy             = errors.New("cache: too many concurrent loads")
	ErrUnknownNamespace = errors.New("cache: unknown namespace")
	ErrUnavailable      = errors.New("cache: shared cache unavailable")
	ErrInvalidKey       = errors.New("cache: invalid key")
)

// LoadFunc reads the authoritative value (MongoDB) and returns it encoded.
type LoadFunc func(ctx context.Context) ([]byte, error)

// Namespace describes one kind of cached data. Register every namespace at
// startup; anything unregistered (checkout, carts, stock) cannot be cached.
type Namespace struct {
	Name        string
	Schema      string        // part of the key; change it when the cached shape changes. Default "v1".
	TTL         time.Duration // hard expiry in Redis
	NegativeTTL time.Duration // how long "not found" is cached; 0 = never
	SoftTTL     time.Duration // stale-while-revalidate after this age; 0 = off. Public data only.
	JitterPct   float64       // random +/- fraction applied to TTL and SoftTTL (0.1 = 10%)
	DegradedTTL time.Duration // in-process reuse while Redis is down; 0 = none
}

// Config holds the limits that protect Redis and MongoDB.
type Config struct {
	Prefix        string
	LockTTL       time.Duration // must exceed LoadTimeout
	LoadTimeout   time.Duration // one database load
	FlightTimeout time.Duration // whole resolve: waiting + loading
	PollBase      time.Duration
	PollMax       time.Duration
	MaxPolls      int // waiting budget before the controlled database fallback
	MaxWaiters    int // requests allowed to wait on loads at once (per instance)
	MaxLoads      int // concurrent database loads (per instance)
	MaxBackground int // concurrent stale-while-revalidate refreshes
	MaxValueBytes int // larger values are not cached (Redis memory guard)
	LocalEntries  int // size of the in-process outage cache
}

func DefaultConfig() Config {
	return Config{
		Prefix:        "shopbd:cache:",
		LockTTL:       10 * time.Second,
		LoadTimeout:   5 * time.Second,
		FlightTimeout: 15 * time.Second,
		PollBase:      10 * time.Millisecond,
		PollMax:       250 * time.Millisecond,
		MaxPolls:      40,
		MaxWaiters:    2000,
		MaxLoads:      16,
		MaxBackground: 4,
		MaxValueBytes: 256 << 10,
		LocalEntries:  512,
	}
}

func (c *Config) normalize() {
	d := DefaultConfig()
	if c.Prefix == "" {
		c.Prefix = d.Prefix
	}
	if c.LockTTL <= 0 {
		c.LockTTL = d.LockTTL
	}
	if c.LoadTimeout <= 0 {
		c.LoadTimeout = d.LoadTimeout
	}
	if c.FlightTimeout <= 0 {
		c.FlightTimeout = d.FlightTimeout
	}
	if c.PollBase <= 0 {
		c.PollBase = d.PollBase
	}
	if c.PollMax < c.PollBase {
		c.PollMax = max(d.PollMax, c.PollBase)
	}
	if c.MaxPolls < 1 {
		c.MaxPolls = d.MaxPolls
	}
	if c.MaxWaiters < 1 {
		c.MaxWaiters = d.MaxWaiters
	}
	if c.MaxLoads < 1 {
		c.MaxLoads = d.MaxLoads
	}
	if c.MaxBackground < 1 {
		c.MaxBackground = d.MaxBackground
	}
	if c.MaxValueBytes < 1 {
		c.MaxValueBytes = d.MaxValueBytes
	}
	if c.LocalEntries < 1 {
		c.LocalEntries = d.LocalEntries
	}
}

type target struct {
	ns      Namespace
	dataKey string
	lockKey string
	verKey  string
}

// Cache is safe for concurrent use. Create one per process.
type Cache struct {
	be  Backend
	cfg Config
	log *slog.Logger
	br  *redisx.Breaker
	nss map[string]Namespace

	verKeep time.Duration // how long a bumped version key must live
	sf      singleflight.Group
	waiters atomic.Int64
	gate    chan struct{}
	bg      chan struct{}
	local   *localStore
	Stats   Stats

	retryMu  sync.Mutex
	pending  map[string]struct{}
	retrying bool

	now func() time.Time
}

// New builds a Cache. be may be nil when Redis is not configured: the cache
// then runs in permanent degraded mode (singleflight + gate + local reuse).
func New(be Backend, br *redisx.Breaker, cfg Config, log *slog.Logger) *Cache {
	cfg.normalize()
	if br == nil {
		br = redisx.NewBreaker(3, 5*time.Second)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Cache{
		be: be, cfg: cfg, log: log, br: br,
		nss:     map[string]Namespace{},
		verKeep: time.Hour,
		gate:    make(chan struct{}, cfg.MaxLoads),
		bg:      make(chan struct{}, cfg.MaxBackground),
		local:   newLocalStore(cfg.LocalEntries),
		pending: map[string]struct{}{},
		now:     time.Now,
	}
}

// Register adds a namespace. Call it at startup, before serving requests.
func (c *Cache) Register(ns Namespace) error {
	if ns.Name == "" || strings.ContainsAny(ns.Name, ": \t\r\n") {
		return fmt.Errorf("cache: invalid namespace name %q", ns.Name)
	}
	if ns.Schema == "" {
		ns.Schema = "v1"
	}
	switch {
	case ns.TTL <= 0:
		return fmt.Errorf("cache: namespace %s needs a TTL", ns.Name)
	case ns.NegativeTTL < 0 || ns.SoftTTL < 0 || ns.DegradedTTL < 0:
		return fmt.Errorf("cache: namespace %s has a negative duration", ns.Name)
	case ns.SoftTTL >= ns.TTL && ns.SoftTTL != 0:
		return fmt.Errorf("cache: namespace %s: SoftTTL must be below TTL", ns.Name)
	case ns.JitterPct < 0 || ns.JitterPct > 0.5:
		return fmt.Errorf("cache: namespace %s: JitterPct must be within 0..0.5", ns.Name)
	}
	c.nss[ns.Name] = ns
	if k := 2 * ns.TTL; k > c.verKeep {
		c.verKeep = k
	}
	return nil
}

/* ---------------------------- reading ---------------------------- */

const (
	stateMiss = iota
	stateHit
	stateStale
)

type envelope struct {
	ver    string
	neg    bool
	softAt int64 // unix ms; 0 = never stale
	body   []byte
}

func encode(e envelope) []byte {
	kind := "v"
	if e.neg {
		kind = "n"
	}
	h := e.ver + "|" + kind + "|" + strconv.FormatInt(e.softAt, 10) + "\n"
	out := make([]byte, 0, len(h)+len(e.body))
	out = append(out, h...)
	return append(out, e.body...)
}

func decode(b []byte) (envelope, bool) {
	i := -1
	for j, ch := range b {
		if ch == '\n' {
			i = j
			break
		}
	}
	if i < 0 {
		return envelope{}, false
	}
	parts := strings.Split(string(b[:i]), "|")
	if len(parts) != 3 {
		return envelope{}, false
	}
	soft, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return envelope{}, false
	}
	return envelope{ver: parts[0], neg: parts[1] == "n", softAt: soft, body: b[i+1:]}, true
}

type readResult struct {
	env   envelope
	state int
	ver   string // current version tag
}

// read fetches the value and its version tag in one round trip. An entry
// written under an older version is a miss: it was invalidated.
func (c *Cache) read(ctx context.Context, t target) (readResult, error) {
	if c.be == nil {
		return readResult{}, errors.New("cache: no backend")
	}
	vals, err := c.be.MGet(ctx, t.dataKey, t.verKey)
	if err != nil {
		c.redisFail(ctx, err)
		return readResult{}, err
	}
	c.br.Success()
	ver := "0"
	if len(vals) == 2 && vals[1] != nil {
		ver = string(vals[1])
	}
	rr := readResult{ver: ver, state: stateMiss}
	if len(vals) == 0 || vals[0] == nil {
		return rr, nil
	}
	env, ok := decode(vals[0])
	if !ok {
		c.Stats.DecodeErrors.Add(1)
		return rr, nil
	}
	if env.ver != ver {
		return rr, nil // written before an invalidation
	}
	rr.env = env
	rr.state = stateHit
	if env.softAt > 0 && c.now().UnixMilli() > env.softAt {
		rr.state = stateStale
	}
	return rr, nil
}

func (c *Cache) redisFail(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return // our own deadline or cancellation, not Redis misbehaving
	}
	c.br.Failure()
	c.Stats.RedisErrors.Add(1)
	c.log.Warn("cache: redis error", "error", err.Error())
}

/* ----------------------------- Fetch ----------------------------- */

// Fetch returns the cached bytes for key, loading them at most once across
// all instances when they are missing. tag is the version tag used for
// invalidation (default: the namespace name); pass "product:<id>" for per-item
// invalidation. The returned slice is shared: callers must not modify it.
func (c *Cache) Fetch(ctx context.Context, nsName, key, tag string, load LoadFunc) ([]byte, error) {
	t, err := c.target(nsName, key, tag)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.be != nil && c.br.Allow() {
		if rr, err := c.read(ctx, t); err == nil {
			switch rr.state {
			case stateHit:
				return c.answer(rr.env, false)
			case stateStale:
				c.refreshAsync(t, load)
				return c.answer(rr.env, true)
			}
		}
	}
	return c.coalesce(ctx, t, load)
}

func (c *Cache) answer(e envelope, stale bool) ([]byte, error) {
	if stale {
		c.Stats.StaleServed.Add(1)
	}
	if e.neg {
		c.Stats.NegativeHits.Add(1)
		return nil, ErrNotFound
	}
	c.Stats.Hits.Add(1)
	return e.body, nil
}

func (c *Cache) coalesce(ctx context.Context, t target, load LoadFunc) ([]byte, error) {
	if c.waiters.Add(1) > int64(c.cfg.MaxWaiters) {
		c.waiters.Add(-1)
		c.Stats.WaiterRejected.Add(1)
		return nil, ErrBusy
	}
	defer c.waiters.Add(-1)

	ch := c.sf.DoChan(t.dataKey, func() (any, error) {
		b, err := c.resolve(t, load, false)
		return b, err
	})
	select {
	case r := <-ch:
		if r.Shared {
			c.Stats.Coalesced.Add(1)
			c.Stats.DBQueriesAvoided.Add(1)
		}
		if r.Err != nil {
			return nil, r.Err
		}
		b, _ := r.Val.([]byte)
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// resolve runs once per key per instance (inside singleflight). It uses its
// own context: one client hanging up must not fail the load for everyone
// else waiting on it. refresh=true is the background revalidation path.
func (c *Cache) resolve(t target, load LoadFunc, refresh bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.FlightTimeout)
	defer cancel()

	if c.be == nil || !c.br.Allow() {
		return c.degraded(ctx, t, load)
	}
	token := newToken()
	contended := false
	for attempt := 0; ; attempt++ {
		rr, err := c.read(ctx, t)
		if err != nil {
			return c.degraded(ctx, t, load)
		}
		if usable(rr.state, refresh) {
			c.Stats.DBQueriesAvoided.Add(1)
			return c.answer(rr.env, false)
		}
		got, err := c.be.TryLock(ctx, t.lockKey, token, c.cfg.LockTTL)
		if err != nil {
			c.redisFail(ctx, err)
			return c.degraded(ctx, t, load)
		}
		if got {
			return c.lead(ctx, t, token, load, refresh)
		}
		if refresh {
			return nil, nil // someone else is already refreshing this key
		}
		if !contended {
			contended = true
			c.Stats.LockContended.Add(1)
		}
		if attempt >= c.cfg.MaxPolls {
			c.Stats.WaitTimeouts.Add(1)
			return c.fallback(ctx, t, load)
		}
		c.Stats.PollWaits.Add(1)
		if err := sleep(ctx, c.backoff(attempt)); err != nil {
			return nil, err
		}
	}
}

func usable(state int, refresh bool) bool {
	return state == stateHit || (state == stateStale && !refresh)
}

// lead runs on the one request that holds the lock.
func (c *Cache) lead(ctx context.Context, t target, token string, load LoadFunc, refresh bool) ([]byte, error) {
	c.Stats.LocksAcquired.Add(1)
	defer c.unlock(t, token)

	// Double check: the previous loader may have finished between our miss
	// and our lock. This read also yields the version to guard the write.
	rr, err := c.read(ctx, t)
	if err != nil {
		return c.degraded(ctx, t, load)
	}
	if usable(rr.state, refresh) {
		c.Stats.DoubleCheckHits.Add(1)
		c.Stats.DBQueriesAvoided.Add(1)
		return c.answer(rr.env, false)
	}
	return c.loadAndStore(ctx, t, load, rr.ver, true)
}

// fallback is the controlled path after the whole waiting budget is spent
// (lock holder stuck or very slow). It is still gated and still per-key
// singleflight, so it is bounded, not a flood.
func (c *Cache) fallback(ctx context.Context, t target, load LoadFunc) ([]byte, error) {
	rr, err := c.read(ctx, t)
	if err != nil {
		return c.degraded(ctx, t, load)
	}
	if usable(rr.state, false) {
		c.Stats.DBQueriesAvoided.Add(1)
		return c.answer(rr.env, false)
	}
	return c.loadAndStore(ctx, t, load, rr.ver, true)
}

func (c *Cache) loadAndStore(ctx context.Context, t target, load LoadFunc, ver string, canStore bool) ([]byte, error) {
	c.Stats.Misses.Add(1)
	val, err := c.gatedLoad(ctx, load)
	switch {
	case errors.Is(err, ErrNotFound):
		if canStore && t.ns.NegativeTTL > 0 {
			c.store(ctx, t, ver, nil, true, jitter(t.ns.NegativeTTL, t.ns.JitterPct))
		}
		return nil, ErrNotFound
	case err != nil:
		return nil, err // errors are never cached
	}
	if canStore {
		c.store(ctx, t, ver, val, false, jitter(t.ns.TTL, t.ns.JitterPct))
	}
	return val, nil
}

// store writes with the compare-and-set on the version captured before the
// database read. Failure to populate is not fatal: the caller still gets the
// data and the next request simply loads again.
func (c *Cache) store(ctx context.Context, t target, ver string, val []byte, neg bool, ttl time.Duration) {
	if len(val) > c.cfg.MaxValueBytes {
		c.Stats.OversizeSkipped.Add(1)
		return
	}
	env := envelope{ver: ver, neg: neg, body: val}
	if !neg && t.ns.SoftTTL > 0 {
		env.softAt = c.now().Add(jitter(t.ns.SoftTTL, t.ns.JitterPct)).UnixMilli()
	}
	// Detach from the flight deadline so a nearly-expired flight can still
	// populate the cache; bound it so it cannot hang.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	ok, err := c.be.SetIfVersion(sctx, t.dataKey, encode(env), ttl, t.verKey, ver)
	if err != nil {
		c.redisFail(sctx, err)
		c.Stats.PopulateErrors.Add(1)
		return
	}
	if !ok {
		c.Stats.StaleWritesDrop.Add(1) // an update invalidated this key mid-load
	}
}

// gatedLoad is the only place a LoadFunc is called. The gate caps concurrent
// database loads per instance; it never queues, so memory and goroutines stay
// bounded under any Redis or lock failure.
func (c *Cache) gatedLoad(ctx context.Context, load LoadFunc) ([]byte, error) {
	select {
	case c.gate <- struct{}{}:
	default:
		c.Stats.GateRejected.Add(1)
		return nil, ErrBusy
	}
	defer func() { <-c.gate }()
	lctx, cancel := context.WithTimeout(ctx, c.cfg.LoadTimeout)
	defer cancel()
	c.Stats.DBLoads.Add(1)
	return load(lctx)
}

// degraded serves while Redis is unusable. Identical concurrent requests are
// already collapsed by singleflight; the gate caps the rest; the small local
// store absorbs repeats for Namespace.DegradedTTL.
func (c *Cache) degraded(ctx context.Context, t target, load LoadFunc) ([]byte, error) {
	if t.ns.DegradedTTL > 0 {
		if v, neg, ok := c.local.get(t.dataKey, c.now()); ok {
			c.Stats.DBQueriesAvoided.Add(1)
			if neg {
				return nil, ErrNotFound
			}
			return v, nil
		}
	}
	c.Stats.DegradedLoads.Add(1)
	epoch := c.local.epoch()
	val, err := c.gatedLoad(ctx, load)
	if errors.Is(err, ErrNotFound) {
		if t.ns.DegradedTTL > 0 {
			c.local.set(t.dataKey, nil, true, t.ns.DegradedTTL, c.now(), epoch)
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if t.ns.DegradedTTL > 0 && len(val) <= c.cfg.MaxValueBytes {
		c.local.set(t.dataKey, val, false, t.ns.DegradedTTL, c.now(), epoch)
	}
	return val, nil
}

// unlock releases only our own lock (token compare in Lua). If it fails the
// lock simply expires after LockTTL.
func (c *Cache) unlock(t target, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.be.Unlock(ctx, t.lockKey, token); err != nil {
		c.redisFail(ctx, err)
	}
}

/* --------------------- stale-while-revalidate --------------------- */

func (c *Cache) refreshAsync(t target, load LoadFunc) {
	select {
	case c.bg <- struct{}{}:
	default:
		return // refreshes are best effort and bounded
	}
	go func() {
		defer func() { <-c.bg }()
		c.Stats.BackgroundRefresh.Add(1)
		<-c.sf.DoChan("swr:"+t.dataKey, func() (any, error) {
			b, err := c.resolve(t, load, true)
			return b, err
		})
	}()
}

/* ------------------------- invalidation ------------------------- */

// Invalidate makes every entry under the given version tags invalid. Call it
// AFTER the database write succeeded. Entries being loaded right now are
// protected too: their write-back sees the new version and is dropped.
//
// If Redis cannot be reached, the tags are retried in the background for a few
// minutes. Until that succeeds, other instances may still serve the old copy
// for at most the namespace TTL, which is why TTLs for mutable public data are
// short.
func (c *Cache) Invalidate(ctx context.Context, tags ...string) error {
	c.local.clear()
	if c.be == nil {
		return nil
	}
	var failed []string
	var first error
	for _, tag := range tags {
		if err := c.be.BumpVersion(ctx, c.cfg.Prefix+"v:"+tag, c.verKeep); err != nil {
			failed = append(failed, tag)
			if first == nil {
				first = err
			}
		}
	}
	if len(failed) > 0 {
		c.Stats.InvalidateErrors.Add(int64(len(failed)))
		c.log.Error("cache: invalidation failed, will retry", "tags", failed, "error", first.Error())
		c.queueRetry(failed)
		return first
	}
	return nil
}

// Version returns the current version of a tag ("0" if it was never
// invalidated). In-process caches use it to notice that another API instance
// invalidated something. It returns ErrUnavailable without touching Redis when
// the breaker is open, so callers can simply keep their local copy.
func (c *Cache) Version(ctx context.Context, tag string) (string, error) {
	if c.be == nil || !c.br.Allow() {
		return "", ErrUnavailable
	}
	vals, err := c.be.MGet(ctx, c.cfg.Prefix+"v:"+tag)
	if err != nil {
		c.redisFail(ctx, err)
		return "", err
	}
	c.br.Success()
	if len(vals) == 1 && vals[0] != nil {
		return string(vals[0]), nil
	}
	return "0", nil
}

func (c *Cache) queueRetry(tags []string) {
	c.retryMu.Lock()
	defer c.retryMu.Unlock()
	for _, t := range tags {
		if len(c.pending) < 1024 {
			c.pending[t] = struct{}{}
		}
	}
	if c.retrying {
		return
	}
	c.retrying = true
	go c.retryLoop()
}

func (c *Cache) retryLoop() {
	delay := 250 * time.Millisecond
	for attempt := 0; attempt < 40; attempt++ {
		time.Sleep(delay)
		if delay < 8*time.Second {
			delay *= 2
		}
		c.retryMu.Lock()
		tags := make([]string, 0, len(c.pending))
		for t := range c.pending {
			tags = append(tags, t)
		}
		c.retryMu.Unlock()
		if len(tags) == 0 {
			break
		}
		for _, tag := range tags {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := c.be.BumpVersion(ctx, c.cfg.Prefix+"v:"+tag, c.verKeep)
			cancel()
			if err == nil {
				c.retryMu.Lock()
				delete(c.pending, tag)
				c.retryMu.Unlock()
			}
		}
	}
	c.retryMu.Lock()
	if n := len(c.pending); n > 0 {
		c.log.Error("cache: gave up invalidating tags; entries expire by TTL", "count", n)
		c.pending = map[string]struct{}{}
	}
	c.retrying = false
	c.retryMu.Unlock()
}

/* ------------------------------ helpers ------------------------------ */

func (c *Cache) target(nsName, key, tag string) (target, error) {
	ns, ok := c.nss[nsName]
	if !ok {
		return target{}, fmt.Errorf("%w: %q", ErrUnknownNamespace, nsName)
	}
	if key == "" {
		return target{}, ErrInvalidKey
	}
	long := len(key) > 200
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return target{}, ErrInvalidKey
		}
	}
	if long {
		sum := sha256.Sum256([]byte(key))
		key = "h-" + hex.EncodeToString(sum[:16])
	}
	if tag == "" {
		tag = ns.Name
	}
	suffix := ns.Name + ":" + ns.Schema + ":" + key
	return target{
		ns:      ns,
		dataKey: c.cfg.Prefix + "d:" + suffix,
		lockKey: c.cfg.Prefix + "l:" + suffix,
		verKey:  c.cfg.Prefix + "v:" + tag,
	}, nil
}

// ScopedKey builds a cache key for seller-owned data. The seller id is
// mandatory and always the first component, so two sellers can never share a
// key, whatever the remaining parts are.
func ScopedKey(sellerID string, parts ...string) (string, error) {
	if sellerID == "" || strings.ContainsAny(sellerID, ":|\n ") {
		return "", ErrInvalidKey
	}
	return "s=" + sellerID + ":" + strings.Join(parts, ":"), nil
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cache: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// backoff is exponential with full jitter: waiters spread out instead of
// polling Redis in lockstep.
func (c *Cache) backoff(attempt int) time.Duration {
	d := c.cfg.PollBase << min(attempt, 8)
	if d > c.cfg.PollMax || d <= 0 {
		d = c.cfg.PollMax
	}
	return c.cfg.PollBase/2 + time.Duration(mrand.Int64N(int64(d)))
}

func sleep(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// jitter spreads expiry times by +/- pct so keys written together do not all
// expire together.
func jitter(d time.Duration, pct float64) time.Duration {
	if pct <= 0 {
		return d
	}
	f := 1 + (mrand.Float64()*2-1)*pct
	return time.Duration(float64(d) * f)
}

// Get is the typed wrapper: it JSON-encodes what load returns and decodes what
// comes back. Return cache.ErrNotFound from load for a missing item.
func Get[T any](ctx context.Context, c *Cache, ns, key, tag string, load func(context.Context) (T, error)) (T, error) {
	var zero T
	b, err := c.Fetch(ctx, ns, key, tag, func(ctx context.Context) ([]byte, error) {
		v, err := load(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	})
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		// Unreadable cached bytes (shape changed without a Schema bump):
		// answer from the database, through the gate.
		c.Stats.DecodeErrors.Add(1)
		var v T
		_, lerr := c.gatedLoad(ctx, func(ctx context.Context) ([]byte, error) {
			var e error
			v, e = load(ctx)
			return nil, e
		})
		if lerr != nil {
			return zero, lerr
		}
		return v, nil
	}
	return out, nil
}

/* ------------- bounded in-process store (Redis outage only) ------------- */

type localEntry struct {
	val []byte
	neg bool
	exp time.Time
}

type localStore struct {
	mu  sync.Mutex
	m   map[string]localEntry
	max int
	ep  uint64
}

func newLocalStore(max int) *localStore {
	return &localStore{m: make(map[string]localEntry), max: max}
}

func (l *localStore) get(key string, now time.Time) ([]byte, bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[key]
	if !ok || !now.Before(e.exp) {
		delete(l.m, key)
		return nil, false, false
	}
	return e.val, e.neg, true
}

func (l *localStore) epoch() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ep
}

// set stores only if no clear() happened since the load began (epoch check),
// so an invalidation during a load is not undone.
func (l *localStore) set(key string, val []byte, neg bool, ttl time.Duration, now time.Time, epoch uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if epoch != l.ep {
		return
	}
	if len(l.m) >= l.max {
		for k, e := range l.m { // drop expired first, then any one entry
			if !now.Before(e.exp) {
				delete(l.m, k)
			}
		}
		for k := range l.m {
			if len(l.m) < l.max {
				break
			}
			delete(l.m, k)
		}
	}
	l.m[key] = localEntry{val: val, neg: neg, exp: now.Add(ttl)}
}

func (l *localStore) clear() {
	l.mu.Lock()
	l.m = make(map[string]localEntry)
	l.ep++
	l.mu.Unlock()
}
