package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scs32/tailterm/hub/internal/store"
)

// Request timing. The hub prints one line for each request slower than a
// threshold and one summary line a minute, so a slow hour can say whether the
// time went to admission, the store or the work. Lines carry only the method,
// the route pattern, the status and durations: never a path value, query,
// header, body, token, caller key, agent or run ID.
//
// The admission split uses the admitted hook in ServeHTTP, which runs right
// after a request takes its caller slot. That hook was test-only; the timing
// install now sets it in production too, and chains any hook set before it.

// SlowRequestThresholdEnv names the slow-line threshold variable.
const SlowRequestThresholdEnv = "TAILTERM_SLOW_REQUEST_THRESHOLD"

const (
	defaultSlowRequestThreshold = time.Second
	defaultSlowLinesPerMinute   = 60
	timingMinute                = time.Minute
)

// RequestTimingThreshold reads the slow-line threshold: a Go duration, 1 s
// when unset.
func RequestTimingThreshold(getenv func(string) string) (time.Duration, error) {
	raw := getenv(SlowRequestThresholdEnv)
	if raw == "" {
		return defaultSlowRequestThreshold, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration: %q", SlowRequestThresholdEnv, raw)
	}
	return d, nil
}

// RequestTimingConfig configures WithRequestTiming. Zero values take the
// defaults: a 1 s threshold, 60 slow lines a minute, the server's log, the
// real clock and no store counters.
type RequestTimingConfig struct {
	Threshold      time.Duration
	LinesPerMinute int
	Logf           func(string, ...any)
	Now            func() time.Time
	// Stats reads the store's cumulative pool and write-lock counters.
	Stats func() store.PoolStats
}

// RequestTiming is the timed handler and its minute accumulators.
type RequestTiming struct {
	s   *Server
	cfg RequestTimingConfig

	mu sync.Mutex
	// This minute's timed requests.
	requests, slow  int64
	total, admitted time.Duration
	slowestRoute    string
	slowest         time.Duration
	minuteHeldBack  int64
	// The slow-line rate limit: lines printed in the window that began at
	// windowStart, and lines held back since the last printed one.
	windowStart time.Time
	windowLines int
	heldBack    int64
	// base is the store counters at the last minute line.
	base store.PoolStats
}

type timingKey struct{}

// timingRecord is one request's admission stamp. Only the request's own
// goroutine touches it: ServeHTTP runs the admitted hook on that goroutine.
type timingRecord struct {
	admittedAt time.Time
	admitted   bool
}

// WithRequestTiming installs the admission stamp on s and returns the timed
// handler to serve instead of s. Call it once, before s serves, after any
// test sets s.admitted.
func (s *Server) WithRequestTiming(cfg RequestTimingConfig) (http.Handler, *RequestTiming) {
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaultSlowRequestThreshold
	}
	if cfg.LinesPerMinute <= 0 {
		cfg.LinesPerMinute = defaultSlowLinesPerMinute
	}
	if cfg.Logf == nil {
		cfg.Logf = s.logf
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	t := &RequestTiming{s: s, cfg: cfg}
	if cfg.Stats != nil {
		t.base = cfg.Stats()
	}
	prev := s.admitted
	s.admitted = func(key string, r *http.Request) {
		// Stamp before a chained hook, so a hook that holds the request
		// counts as work, not admission wait.
		if rec, ok := r.Context().Value(timingKey{}).(*timingRecord); ok {
			rec.admittedAt, rec.admitted = cfg.Now(), true
		}
		if prev != nil {
			prev(key, r)
		}
	}
	return t, t
}

// ServeHTTP times one request. The events long polls are served untimed and
// uncounted: they wait by design.
func (t *RequestTiming) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, pattern := t.s.mux.Handler(r)
	if admissionExempt[pattern] {
		t.s.ServeHTTP(w, r)
		return
	}
	rec := &timingRecord{}
	sw := &statusWriter{ResponseWriter: w}
	start := t.cfg.Now()
	defer func() { t.finish(r.Method, pattern, sw.status, start, rec) }()
	t.s.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), timingKey{}, rec)))
	if sw.status == 0 {
		// Nothing written: net/http sends 200. Only a panic leaves it unset.
		sw.status = http.StatusOK
	}
}

func (t *RequestTiming) finish(method, pattern string, status int, start time.Time, rec *timingRecord) {
	end := t.cfg.Now()
	total := end.Sub(start)
	var wait time.Duration
	if rec.admitted {
		wait = rec.admittedAt.Sub(start)
	}
	route := "unmatched"
	if pattern != "" {
		route = strconv.Quote(pattern)
	}
	t.mu.Lock()
	t.requests++
	t.total += total
	t.admitted += wait
	if total > t.slowest || t.slowestRoute == "" {
		t.slowest, t.slowestRoute = total, route
	}
	if total <= t.cfg.Threshold {
		t.mu.Unlock()
		return
	}
	t.slow++
	if t.windowStart.IsZero() || end.Sub(t.windowStart) >= timingMinute {
		t.windowStart, t.windowLines = end, 0
	}
	if t.windowLines >= t.cfg.LinesPerMinute {
		t.heldBack++
		t.minuteHeldBack++
		t.mu.Unlock()
		return
	}
	t.windowLines++
	held := t.heldBack
	t.heldBack = 0
	t.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "hub slow request method=%s route=%s status=%s total_ms=%d", logMethod(method), route, logStatus(status), total.Milliseconds())
	if rec.admitted {
		fmt.Fprintf(&b, " admission_ms=%d admitted=yes", wait.Milliseconds())
	} else {
		b.WriteString(" admitted=no")
	}
	if held > 0 {
		fmt.Fprintf(&b, " held_back=%d", held)
	}
	t.cfg.Logf("%s", b.String())
}

// Run prints the minute line once a minute until ctx ends.
func (t *RequestTiming) Run(ctx context.Context) {
	ticker := time.NewTicker(timingMinute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.flush()
		}
	}
}

// flush prints the minute line and starts the next minute. It prints nothing
// when no timed request finished since the last line.
func (t *RequestTiming) flush() {
	var stats store.PoolStats
	if t.cfg.Stats != nil {
		stats = t.cfg.Stats()
	}
	t.mu.Lock()
	base := t.base
	t.base = stats
	if t.requests == 0 {
		t.mu.Unlock()
		return
	}
	requests, slow, total, admitted := t.requests, t.slow, t.total, t.admitted
	slowestRoute, slowest, held := t.slowestRoute, t.slowest, t.minuteHeldBack
	t.requests, t.slow, t.total, t.admitted = 0, 0, 0, 0
	t.slowestRoute, t.slowest, t.minuteHeldBack = "", 0, 0
	t.mu.Unlock()

	inFlight, waiting := t.s.gate.load()
	var b strings.Builder
	fmt.Fprintf(&b, "hub requests minute requests=%d slow=%d total_ms=%d admission_ms=%d slowest_route=%s slowest_ms=%d inflight=%d waiting=%d",
		requests, slow, total.Milliseconds(), admitted.Milliseconds(), slowestRoute, slowest.Milliseconds(), inFlight, waiting)
	if t.cfg.Stats != nil {
		fmt.Fprintf(&b, " db_waits=%d db_wait_ms=%d db_in_use=%d write_locks=%d write_lock_wait_ms=%d",
			stats.WaitCount-base.WaitCount, (stats.WaitDuration - base.WaitDuration).Milliseconds(), stats.InUse,
			stats.WriteLocks-base.WriteLocks, (stats.WriteLockWait - base.WriteLockWait).Milliseconds())
	}
	fmt.Fprintf(&b, " slow_held_back=%d", held)
	t.cfg.Logf("%s", b.String())
}

// load counts the admitted requests in flight and the requests waiting for a
// slot now. Exempt long polls are never counted.
func (a *admission) load() (inFlight, waiting int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range a.callers {
		inFlight += l.inFlight
	}
	return inFlight, len(a.queue)
}

// logMethod keeps client-chosen method text out of the log.
func logMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return method
	}
	return "OTHER"
}

func logStatus(status int) string {
	if status == 0 {
		return "none"
	}
	return strconv.Itoa(status)
}

// statusWriter records the response status.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap keeps http.ResponseController working through the wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
