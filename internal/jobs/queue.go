// Package jobs runs work in the background: a bounded in-process queue with a
// fixed pool of workers and delayed retries.
//
// It is deliberately in memory. A job may carry a secret (a password reset
// code on its way to an inbox), and writing that to a database just to survive
// a restart would be worse than losing the job: the person can ask again.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Job is one unit of background work.
type Job struct {
	// Name labels the job in logs. Never put secrets or personal data in it.
	Name string
	// Run does the work. A non-nil error makes the queue retry the job, up to
	// MaxAttempts. Return nil for failures that must not be retried.
	Run func(ctx context.Context) error
	// MaxAttempts is the total number of tries; 0 or 1 means no retry.
	MaxAttempts int
	// Backoff[n-1] is the wait before retry n; the last value repeats.
	// Empty means DefaultBackoff.
	Backoff []time.Duration
	// Timeout limits one attempt; 0 means DefaultTimeout.
	Timeout time.Duration
}

const DefaultTimeout = 30 * time.Second

var DefaultBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

type task struct {
	job     Job
	attempt int // attempts already made
}

// Queue is a bounded queue served by a fixed set of workers.
type Queue struct {
	log    *slog.Logger
	ch     chan *task
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	timers map[*time.Timer]struct{} // retries waiting for their delay
}

// New starts `workers` goroutines reading from a queue that holds `buffer`
// waiting jobs.
func New(workers, buffer int, log *slog.Logger) *Queue {
	if workers < 1 {
		workers = 1
	}
	if buffer < 1 {
		buffer = 1
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{
		log:    log,
		ch:     make(chan *task, buffer),
		ctx:    ctx,
		cancel: cancel,
		timers: map[*time.Timer]struct{}{},
	}
	q.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go q.worker()
	}
	return q
}

// Enqueue adds a job without ever blocking the caller. It reports false when
// the queue is full or already shut down; the job is then not run.
func (q *Queue) Enqueue(j Job) bool {
	if j.Run == nil {
		return false
	}
	return q.push(&task{job: j})
}

func (q *Queue) push(t *task) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	select {
	case q.ch <- t:
		return true
	default:
		return false
	}
}

func (q *Queue) worker() {
	defer q.wg.Done()
	for t := range q.ch {
		q.run(t)
	}
}

func (q *Queue) run(t *task) {
	timeout := t.job.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(q.ctx, timeout)
	defer cancel()

	err := q.safeRun(ctx, t.job)
	if err == nil {
		return
	}
	t.attempt++
	if t.attempt >= t.job.MaxAttempts {
		q.log.Error("background job failed", "job", t.job.Name, "attempts", t.attempt, "error", err.Error())
		return
	}
	delay := backoffFor(t.job.Backoff, t.attempt)
	q.log.Warn("background job failed, will retry", "job", t.job.Name,
		"attempt", t.attempt, "retry_in", delay.String(), "error", err.Error())
	q.retryAfter(t, delay)
}

// safeRun keeps a panicking job from killing its worker.
func (q *Queue) safeRun(ctx context.Context, j Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// The panic value is dropped on purpose: it could contain data
			// from the job's payload.
			err = fmt.Errorf("job panicked")
		}
	}()
	return j.Run(ctx)
}

func backoffFor(b []time.Duration, retry int) time.Duration {
	if len(b) == 0 {
		b = DefaultBackoff
	}
	if retry > len(b) {
		retry = len(b)
	}
	return b[retry-1]
}

// retryAfter puts the task back after delay without holding a worker while it
// waits, so a mail outage cannot freeze the whole pool.
func (q *Queue) retryAfter(t *task, delay time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		q.log.Warn("background job dropped at shutdown", "job", t.job.Name)
		return
	}
	var tm *time.Timer
	// The callback takes q.mu first, so it cannot run before tm is assigned
	// and registered below.
	tm = time.AfterFunc(delay, func() {
		q.mu.Lock()
		delete(q.timers, tm)
		q.mu.Unlock()
		if !q.push(t) {
			q.log.Warn("background job dropped: queue full or shut down", "job", t.job.Name)
		}
	})
	q.timers[tm] = struct{}{}
}

// Shutdown stops accepting jobs, cancels retries that are still waiting, and
// lets queued and running jobs finish. If ctx ends first, running jobs are
// cancelled and ctx's error is returned.
func (q *Queue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		for tm := range q.timers {
			tm.Stop()
		}
		q.timers = map[*time.Timer]struct{}{}
		close(q.ch) // safe: push checks closed under the same lock
	}
	q.mu.Unlock()

	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		q.cancel()
		return nil
	case <-ctx.Done():
		q.cancel()
		return ctx.Err()
	}
}
