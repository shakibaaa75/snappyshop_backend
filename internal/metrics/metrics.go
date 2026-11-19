// Package metrics keeps lightweight in-memory traffic and runtime statistics
// for the admin "System health" page. Nothing is persisted: counters start at
// zero when the server starts and are per server instance.
package metrics

import (
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	windowMinutes = 60  // per-minute history kept
	maxRoutes     = 200 // distinct routes tracked before the rest is merged into "other"
)

// latencyBoundsMs are the upper edges of the latency histogram buckets; the
// last bucket is "slower than the final bound".
var latencyBoundsMs = []int64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}

type minuteBucket struct {
	minute int64 // unix minute this bucket holds
	req    int64
	e4     int64
	e5     int64
	latNs  int64
}

type routeStat struct {
	count int64
	e4    int64
	e5    int64
	latNs int64
	maxNs int64
}

// Collector records requests. The zero value is NOT ready: use New.
type Collector struct {
	start    time.Time
	inFlight atomic.Int64

	mu       sync.Mutex
	total    int64
	bytesOut int64
	latNs    int64
	classes  [6]int64 // index = status/100 (1..5); 0 = other
	hist     []int64  // len(latencyBoundsMs)+1
	buckets  [windowMinutes]minuteBucket
	routes   map[string]*routeStat

	now func() time.Time
}

// Default is the process-wide collector used by the router.
var Default = New()

func New() *Collector {
	return &Collector{
		start:  time.Now(),
		hist:   make([]int64, len(latencyBoundsMs)+1),
		routes: map[string]*routeStat{},
		now:    time.Now,
	}
}

// Observe records one finished request.
func (c *Collector) Observe(route string, status int, bytes int64, d time.Duration) {
	if status == 0 {
		status = http.StatusOK
	}
	class := status / 100
	if class < 1 || class > 5 {
		class = 0
	}
	ns := d.Nanoseconds()
	ms := d.Milliseconds()
	nowMin := c.now().Unix() / 60

	c.mu.Lock()
	defer c.mu.Unlock()

	c.total++
	c.bytesOut += bytes
	c.latNs += ns
	c.classes[class]++

	hi := len(latencyBoundsMs)
	for i, b := range latencyBoundsMs {
		if ms <= b {
			hi = i
			break
		}
	}
	c.hist[hi]++

	b := &c.buckets[nowMin%windowMinutes]
	if b.minute != nowMin {
		*b = minuteBucket{minute: nowMin}
	}
	b.req++
	b.latNs += ns
	switch class {
	case 4:
		b.e4++
	case 5:
		b.e5++
	}

	rs, ok := c.routes[route]
	if !ok {
		if len(c.routes) >= maxRoutes {
			route = "other"
			rs = c.routes[route]
		}
		if rs == nil {
			rs = &routeStat{}
			c.routes[route] = rs
		}
	}
	rs.count++
	rs.latNs += ns
	if ns > rs.maxNs {
		rs.maxNs = ns
	}
	switch class {
	case 4:
		rs.e4++
	case 5:
		rs.e5++
	}
}

// MinutePoint is one minute of traffic.
type MinutePoint struct {
	Minute   string  `json:"minute"` // RFC3339, start of the minute
	Requests int64   `json:"requests"`
	Errors4  int64   `json:"errors4xx"`
	Errors5  int64   `json:"errors5xx"`
	AvgMs    float64 `json:"avgMs"`
}

// RouteRow is the traffic of one route.
type RouteRow struct {
	Route    string  `json:"route"`
	Requests int64   `json:"requests"`
	Errors4  int64   `json:"errors4xx"`
	Errors5  int64   `json:"errors5xx"`
	AvgMs    float64 `json:"avgMs"`
	MaxMs    float64 `json:"maxMs"`
}

// Snapshot is a point-in-time copy of the collected traffic.
type Snapshot struct {
	StartedAt     string  `json:"startedAt"`
	UptimeSeconds int64   `json:"uptimeSeconds"`
	TotalRequests int64   `json:"totalRequests"`
	InFlight      int64   `json:"inFlight"`
	BytesOut      int64   `json:"bytesOut"`
	Status2xx     int64   `json:"status2xx"`
	Status3xx     int64   `json:"status3xx"`
	Status4xx     int64   `json:"status4xx"`
	Status5xx     int64   `json:"status5xx"`
	AvgMs         float64 `json:"avgMs"`
	P50Ms         float64 `json:"p50Ms"`
	P95Ms         float64 `json:"p95Ms"`
	P99Ms         float64 `json:"p99Ms"`

	LastMinute     int64   `json:"lastMinute"`   // requests in the previous full minute
	Last5Minutes   int64   `json:"last5Minutes"` // requests in the last 5 minutes
	Last5Errors5xx int64   `json:"last5Errors5xx"`
	Last5ErrorRate float64 `json:"last5ErrorRate"` // 5xx share over the last 5 minutes, 0-100
	ErrorRate      float64 `json:"errorRate"`      // 5xx share since start, 0-100

	Series     []MinutePoint `json:"series"` // last 60 minutes, oldest first
	TopRoutes  []RouteRow    `json:"topRoutes"`
	SlowRoutes []RouteRow    `json:"slowRoutes"`
}

// Snapshot copies the current numbers.
func (c *Collector) Snapshot() Snapshot {
	nowMin := c.now().Unix() / 60

	c.mu.Lock()
	defer c.mu.Unlock()

	s := Snapshot{
		StartedAt:     c.start.UTC().Format(time.RFC3339),
		UptimeSeconds: int64(c.now().Sub(c.start).Seconds()),
		TotalRequests: c.total,
		InFlight:      c.inFlight.Load(),
		BytesOut:      c.bytesOut,
		Status2xx:     c.classes[2],
		Status3xx:     c.classes[3],
		Status4xx:     c.classes[4],
		Status5xx:     c.classes[5],
	}
	if c.total > 0 {
		s.AvgMs = float64(c.latNs) / float64(c.total) / 1e6
		s.ErrorRate = float64(c.classes[5]) * 100 / float64(c.total)
	}
	s.P50Ms = c.percentile(0.50)
	s.P95Ms = c.percentile(0.95)
	s.P99Ms = c.percentile(0.99)

	s.Series = make([]MinutePoint, 0, windowMinutes)
	for i := windowMinutes - 1; i >= 0; i-- {
		m := nowMin - int64(i)
		p := MinutePoint{Minute: time.Unix(m*60, 0).UTC().Format(time.RFC3339)}
		b := c.buckets[m%windowMinutes]
		if b.minute == m {
			p.Requests, p.Errors4, p.Errors5 = b.req, b.e4, b.e5
			if b.req > 0 {
				p.AvgMs = float64(b.latNs) / float64(b.req) / 1e6
			}
		}
		s.Series = append(s.Series, p)
		switch {
		case i == 1:
			s.LastMinute = p.Requests
		}
		if i < 5 { // current minute + previous 4
			s.Last5Minutes += p.Requests
			s.Last5Errors5xx += p.Errors5
		}
	}
	if s.Last5Minutes > 0 {
		s.Last5ErrorRate = float64(s.Last5Errors5xx) * 100 / float64(s.Last5Minutes)
	}

	rows := make([]RouteRow, 0, len(c.routes))
	for name, rs := range c.routes {
		row := RouteRow{
			Route: name, Requests: rs.count, Errors4: rs.e4, Errors5: rs.e5,
			MaxMs: float64(rs.maxNs) / 1e6,
		}
		if rs.count > 0 {
			row.AvgMs = float64(rs.latNs) / float64(rs.count) / 1e6
		}
		rows = append(rows, row)
	}
	byCount := append([]RouteRow(nil), rows...)
	sort.Slice(byCount, func(i, j int) bool { return byCount[i].Requests > byCount[j].Requests })
	s.TopRoutes = firstN(byCount, 10)

	bySlow := make([]RouteRow, 0, len(rows))
	for _, r := range rows {
		if r.Requests >= 3 { // ignore one-off outliers
			bySlow = append(bySlow, r)
		}
	}
	sort.Slice(bySlow, func(i, j int) bool { return bySlow[i].AvgMs > bySlow[j].AvgMs })
	s.SlowRoutes = firstN(bySlow, 5)
	return s
}

func firstN(rows []RouteRow, n int) []RouteRow {
	if len(rows) > n {
		rows = rows[:n]
	}
	if rows == nil {
		rows = []RouteRow{}
	}
	return rows
}

// percentile estimates a latency percentile (ms) from the histogram: it
// returns the upper edge of the bucket the percentile falls in. Caller holds mu.
func (c *Collector) percentile(p float64) float64 {
	if c.total == 0 {
		return 0
	}
	target := int64(float64(c.total)*p + 0.999999)
	if target < 1 {
		target = 1
	}
	var run int64
	for i, n := range c.hist {
		run += n
		if run >= target {
			if i < len(latencyBoundsMs) {
				return float64(latencyBoundsMs[i])
			}
			return float64(latencyBoundsMs[len(latencyBoundsMs)-1])
		}
	}
	return float64(latencyBoundsMs[len(latencyBoundsMs)-1])
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush keeps streaming responses working.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware counts every request except health probes. routeOf returns the
// route pattern (for example "/api/v1/store/{slug}") after the request ran, so
// requests for different slugs are grouped into one row; it may return "".
func Middleware(c *Collector, routeOf func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			c.inFlight.Add(1)
			sw := &statusWriter{ResponseWriter: w}
			defer func() {
				c.inFlight.Add(-1)
				route := ""
				if routeOf != nil {
					route = routeOf(r)
				}
				if route == "" {
					route = "(unmatched)"
				}
				c.Observe(r.Method+" "+route, sw.status, sw.bytes, time.Since(start))
			}()
			next.ServeHTTP(sw, r)
		})
	}
}
