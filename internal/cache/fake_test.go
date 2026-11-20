package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shopbd/backend/internal/redisx"
)

// fakeBackend behaves like the Redis scripts in backend.go (same compare-and-
// set, same lock ownership rules) and lets tests inject latency, outages and
// write failures.
type fakeBackend struct {
	mu    sync.Mutex
	data  map[string]fakeVal
	vers  map[string]string
	locks map[string]fakeLock
	seq   int64

	down    atomic.Bool
	failSet atomic.Bool
	latency atomic.Int64 // nanoseconds
	mgets   atomic.Int64
}

type fakeVal struct {
	b   []byte
	exp time.Time
}
type fakeLock struct {
	token string
	exp   time.Time
}

func newFake() *fakeBackend {
	return &fakeBackend{data: map[string]fakeVal{}, vers: map[string]string{}, locks: map[string]fakeLock{}}
}

var errDown = errors.New("fake redis: connection refused")

func (f *fakeBackend) pre() error {
	if d := f.latency.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
	if f.down.Load() {
		return errDown
	}
	return nil
}

func (f *fakeBackend) MGet(_ context.Context, keys ...string) ([][]byte, error) {
	f.mgets.Add(1)
	if err := f.pre(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(keys))
	for i, k := range keys {
		if v, ok := f.data[k]; ok && time.Now().Before(v.exp) {
			out[i] = v.b
		} else if s, ok := f.vers[k]; ok {
			out[i] = []byte(s)
		}
	}
	return out, nil
}

func (f *fakeBackend) SetIfVersion(_ context.Context, key string, val []byte, ttl time.Duration, verKey, expect string) (bool, error) {
	if err := f.pre(); err != nil {
		return false, err
	}
	if f.failSet.Load() {
		return false, errors.New("fake redis: OOM")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.vers[verKey]
	if !ok {
		cur = "0"
	}
	if cur != expect {
		return false, nil
	}
	f.data[key] = fakeVal{b: append([]byte(nil), val...), exp: time.Now().Add(ttl)}
	return true, nil
}

func (f *fakeBackend) BumpVersion(_ context.Context, verKey string, _ time.Duration) error {
	if err := f.pre(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.vers[verKey] = fmt.Sprintf("%d", time.Now().UnixMicro()+f.seq)
	return nil
}

func (f *fakeBackend) TryLock(_ context.Context, key, token string, ttl time.Duration) (bool, error) {
	if err := f.pre(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.locks[key]; ok && time.Now().Before(l.exp) {
		return false, nil
	}
	f.locks[key] = fakeLock{token: token, exp: time.Now().Add(ttl)}
	return true, nil
}

func (f *fakeBackend) Unlock(_ context.Context, key, token string) error {
	if err := f.pre(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.locks[key]; ok && l.token == token {
		delete(f.locks, key)
	}
	return nil
}

func (f *fakeBackend) raw(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.data[key]; ok && time.Now().Before(v.exp) {
		return v.b
	}
	return nil
}

func (f *fakeBackend) lockOwner(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.locks[key]; ok && time.Now().Before(l.exp) {
		return l.token
	}
	return ""
}

func (f *fakeBackend) version(verKey string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vers[verKey]
}

// loader counts calls and the highest number of overlapping calls.
type loader struct {
	calls atomic.Int64
	cur   atomic.Int64
	peak  atomic.Int64
	delay time.Duration
	value string
}

func (l *loader) load(ctx context.Context) ([]byte, error) {
	n := l.calls.Add(1)
	c := l.cur.Add(1)
	defer l.cur.Add(-1)
	for {
		p := l.peak.Load()
		if c <= p || l.peak.CompareAndSwap(p, c) {
			break
		}
	}
	select {
	case <-time.After(l.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if l.value == "" {
		return []byte(fmt.Sprintf("v%d", n)), nil
	}
	return []byte(l.value), nil
}

func testCfg() Config {
	return Config{
		Prefix: "t:", LockTTL: 5 * time.Second, LoadTimeout: 2 * time.Second, FlightTimeout: 10 * time.Second,
		PollBase: 5 * time.Millisecond, PollMax: 30 * time.Millisecond, MaxPolls: 200,
		MaxWaiters: 5000, MaxLoads: 16, MaxBackground: 4, MaxValueBytes: 1 << 20, LocalEntries: 256,
	}
}

func newTestCache(t *testing.T, be Backend, mod func(*Config)) *Cache {
	t.Helper()
	cfg := testCfg()
	if mod != nil {
		mod(&cfg)
	}
	c := New(be, redisx.NewBreaker(5, 200*time.Millisecond), cfg, nil)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Register(Namespace{Name: "product", TTL: time.Minute, NegativeTTL: 30 * time.Second, JitterPct: 0.1, DegradedTTL: 2 * time.Second}))
	must(c.Register(Namespace{Name: "page", TTL: time.Minute, SoftTTL: 20 * time.Millisecond}))
	return c
}

// burst starts n goroutines at once and collects results.
func burst(n int, fn func(i int) ([]byte, error)) (vals [][]byte, errs []error) {
	vals = make([][]byte, n)
	errs = make([]error, n)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			vals[i], errs[i] = fn(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return
}
