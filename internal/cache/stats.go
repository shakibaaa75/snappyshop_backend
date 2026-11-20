package cache

import "sync/atomic"

// Stats are cumulative counters for this API instance. Every counter that
// matters for operating the cache is here: stampedes absorbed, lock
// contention, waiting requests, timeouts and database queries avoided.
type Stats struct {
	Hits              atomic.Int64 // served from Redis without any loading
	Misses            atomic.Int64 // requests that needed a load
	NegativeHits      atomic.Int64 // "does not exist" answered from cache
	StaleServed       atomic.Int64 // stale-while-revalidate answers
	BackgroundRefresh atomic.Int64

	Coalesced       atomic.Int64 // requests that shared another request's result (singleflight)
	LocksAcquired   atomic.Int64
	LockContended   atomic.Int64 // requests that found the lock held by someone else
	PollWaits       atomic.Int64 // sleeps while waiting for another instance's load
	WaitTimeouts    atomic.Int64 // waited the whole budget, fell back to the database
	DoubleCheckHits atomic.Int64 // found the value after getting the lock

	DBLoads          atomic.Int64 // loader invocations (database queries issued)
	DBQueriesAvoided atomic.Int64 // requests answered without their own query
	DegradedLoads    atomic.Int64 // loads done while Redis was unusable
	GateRejected     atomic.Int64 // refused: too many concurrent loads
	WaiterRejected   atomic.Int64 // refused: too many waiting requests

	RedisErrors      atomic.Int64
	PopulateErrors   atomic.Int64
	StaleWritesDrop  atomic.Int64 // writes dropped because an update bumped the version
	OversizeSkipped  atomic.Int64
	DecodeErrors     atomic.Int64
	InvalidateErrors atomic.Int64
}

// Snapshot returns a copy suitable for logging or an admin endpoint.
func (s *Stats) Snapshot() map[string]int64 {
	return map[string]int64{
		"hits":               s.Hits.Load(),
		"misses":             s.Misses.Load(),
		"negative_hits":      s.NegativeHits.Load(),
		"stale_served":       s.StaleServed.Load(),
		"background_refresh": s.BackgroundRefresh.Load(),
		"coalesced":          s.Coalesced.Load(),
		"locks_acquired":     s.LocksAcquired.Load(),
		"lock_contended":     s.LockContended.Load(),
		"poll_waits":         s.PollWaits.Load(),
		"wait_timeouts":      s.WaitTimeouts.Load(),
		"double_check_hits":  s.DoubleCheckHits.Load(),
		"db_loads":           s.DBLoads.Load(),
		"db_queries_avoided": s.DBQueriesAvoided.Load(),
		"degraded_loads":     s.DegradedLoads.Load(),
		"gate_rejected":      s.GateRejected.Load(),
		"waiter_rejected":    s.WaiterRejected.Load(),
		"redis_errors":       s.RedisErrors.Load(),
		"populate_errors":    s.PopulateErrors.Load(),
		"stale_writes_drop":  s.StaleWritesDrop.Load(),
		"oversize_skipped":   s.OversizeSkipped.Load(),
		"decode_errors":      s.DecodeErrors.Load(),
		"invalidate_errors":  s.InvalidateErrors.Load(),
	}
}
