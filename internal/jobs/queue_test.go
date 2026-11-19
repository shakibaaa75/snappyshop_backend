package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunsJob(t *testing.T) {
	q := New(2, 4, quiet())
	defer q.Shutdown(context.Background())
	var n int32
	if !q.Enqueue(Job{Name: "ok", Run: func(context.Context) error { atomic.AddInt32(&n, 1); return nil }}) {
		t.Fatal("enqueue refused")
	}
	waitFor(t, "job to run", func() bool { return atomic.LoadInt32(&n) == 1 })
}

func TestRetriesUntilSuccess(t *testing.T) {
	q := New(1, 4, quiet())
	defer q.Shutdown(context.Background())
	var n int32
	q.Enqueue(Job{
		Name: "flaky", MaxAttempts: 4, Backoff: []time.Duration{time.Millisecond},
		Run: func(context.Context) error {
			if atomic.AddInt32(&n, 1) < 3 {
				return errors.New("smtp down")
			}
			return nil
		},
	})
	waitFor(t, "third attempt", func() bool { return atomic.LoadInt32(&n) == 3 })
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt32(&n); got != 3 {
		t.Fatalf("job must stop after success, ran %d times", got)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	q := New(1, 4, quiet())
	defer q.Shutdown(context.Background())
	var n int32
	q.Enqueue(Job{
		Name: "dead", MaxAttempts: 3, Backoff: []time.Duration{time.Millisecond},
		Run: func(context.Context) error { atomic.AddInt32(&n, 1); return errors.New("nope") },
	})
	waitFor(t, "3 attempts", func() bool { return atomic.LoadInt32(&n) == 3 })
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt32(&n); got != 3 {
		t.Fatalf("expected exactly 3 attempts, got %d", got)
	}
}

func TestNilErrorIsNeverRetried(t *testing.T) {
	q := New(1, 4, quiet())
	defer q.Shutdown(context.Background())
	var n int32
	q.Enqueue(Job{Name: "once", MaxAttempts: 5, Backoff: []time.Duration{time.Millisecond},
		Run: func(context.Context) error { atomic.AddInt32(&n, 1); return nil }})
	time.Sleep(40 * time.Millisecond)
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("ran %d times", got)
	}
}

func TestEnqueueNeverBlocksWhenFull(t *testing.T) {
	q := New(1, 1, quiet())
	release := make(chan struct{})
	defer func() { close(release); q.Shutdown(context.Background()) }()
	block := Job{Name: "block", Run: func(context.Context) error { <-release; return nil }}

	q.Enqueue(block) // taken by the worker
	waitFor(t, "worker to pick it up", func() bool { return len(q.ch) == 0 })
	if !q.Enqueue(block) { // fills the single buffer slot
		t.Fatal("buffer slot should accept a job")
	}
	start := time.Now()
	if q.Enqueue(block) {
		t.Fatal("full queue must refuse")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("Enqueue blocked")
	}
}

func TestPanicDoesNotKillWorker(t *testing.T) {
	q := New(1, 4, quiet())
	defer q.Shutdown(context.Background())
	q.Enqueue(Job{Name: "boom", Run: func(context.Context) error { panic("secret-code-123456") }})
	var n int32
	q.Enqueue(Job{Name: "after", Run: func(context.Context) error { atomic.AddInt32(&n, 1); return nil }})
	waitFor(t, "worker to survive", func() bool { return atomic.LoadInt32(&n) == 1 })
}

func TestShutdownDrainsAndRefusesNewJobs(t *testing.T) {
	q := New(1, 8, quiet())
	var n int32
	for i := 0; i < 5; i++ {
		q.Enqueue(Job{Name: "work", Run: func(context.Context) error {
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&n, 1)
			return nil
		}})
	}
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&n); got != 5 {
		t.Fatalf("shutdown must finish queued work, ran %d of 5", got)
	}
	if q.Enqueue(Job{Name: "late", Run: func(context.Context) error { return nil }}) {
		t.Fatal("enqueue after shutdown must be refused")
	}
}

func TestShutdownCancelsPendingRetries(t *testing.T) {
	q := New(1, 4, quiet())
	var n int32
	q.Enqueue(Job{Name: "retry-later", MaxAttempts: 3, Backoff: []time.Duration{50 * time.Millisecond},
		Run: func(context.Context) error { atomic.AddInt32(&n, 1); return errors.New("fail") }})
	waitFor(t, "first attempt", func() bool { return atomic.LoadInt32(&n) == 1 })
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("retry fired after shutdown: %d runs", got)
	}
}

func TestShutdownTimeoutCancelsRunningJobs(t *testing.T) {
	q := New(1, 1, quiet())
	started := make(chan struct{})
	q.Enqueue(Job{Name: "stuck", Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return nil
	}})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := q.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}
