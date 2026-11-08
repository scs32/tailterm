package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

const (
	slowLinePrefix   = "hub slow request "
	minuteLinePrefix = "hub requests minute "
)

// timingLog captures the timing lines a test hub prints.
type timingLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *timingLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *timingLog) with(prefix string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.HasPrefix(line, prefix) {
			out = append(out, line)
		}
	}
	return out
}

func (l *timingLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// timed installs request timing on the client's isolated server and points
// the client at the timed handler. Set c.s.admitted and gate limits first.
func timed(c *client, cfg RequestTimingConfig) (*RequestTiming, *timingLog) {
	c.t.Helper()
	lines := &timingLog{}
	cfg.Logf = lines.logf
	handler, timing := c.s.WithRequestTiming(cfg)
	c.srv = httptest.NewServer(handler)
	c.t.Cleanup(c.srv.Close)
	return timing, lines
}

// field reads key=value from a timing line.
func field(t *testing.T, line, key string) string {
	t.Helper()
	m := regexp.MustCompile(`(?:^| )` + regexp.QuoteMeta(key) + `=("[^"]*"|\S+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("line has no %s: %q", key, line)
	}
	return m[1]
}

func intField(t *testing.T, line, key string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(field(t, line, key), 10, 64)
	if err != nil {
		t.Fatalf("%s is not a number in %q", key, line)
	}
	return n
}

// fakeClock steps 1 ms on every read so each timed request takes time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeClock) read() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(time.Millisecond)
	return f.now
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// T1: a request held behind another caller's admission slot reports an
// admission wait at least as long as it was held.
func TestRequestTimingReportsAdmissionWait(t *testing.T) {
	c := newClient(t)
	c.s.gate.agentTotal = 1
	agentA, agentB := api.NewID("agt"), api.NewID("agt")
	keyA := "agent:" + agentA + "/run_a"
	holdA := make(chan struct{})
	c.s.admitted = func(key string, _ *http.Request) {
		if key == keyA {
			<-holdA
		}
	}
	_, lines := timed(c, RequestTimingConfig{Threshold: time.Millisecond})

	doneA, doneB := make(chan int, 1), make(chan int, 1)
	go func() { code, _, _ := c.getAs(agentA, "run_a", "/v1/whoami", nil); doneA <- code }()
	waitUntil(t, "caller A admitted", func() bool { inFlight, _ := c.gateLoad(keyA); return inFlight == 1 })
	go func() { code, _, _ := c.getAs(agentB, "run_b", "/v1/capabilities", nil); doneB <- code }()
	waitUntil(t, "caller B waiting for the agent lane", func() bool { _, waiting := c.s.gate.load(); return waiting == 1 })
	const hold = 200 * time.Millisecond
	time.Sleep(hold)
	close(holdA)
	if code := <-doneA; code != 200 {
		t.Fatalf("caller A: %d", code)
	}
	if code := <-doneB; code != 200 {
		t.Fatalf("caller B: %d", code)
	}

	var lineA, lineB string
	for _, line := range lines.with(slowLinePrefix) {
		switch field(t, line, "route") {
		case `"GET /v1/whoami"`:
			lineA = line
		case `"GET /v1/capabilities"`:
			lineB = line
		}
	}
	if lineA == "" || lineB == "" {
		t.Fatalf("want a slow line for both callers, got %q", lines.all())
	}
	if field(t, lineB, "admitted") != "yes" || field(t, lineB, "method") != "GET" || field(t, lineB, "status") != "200" {
		t.Fatalf("caller B line: %q", lineB)
	}
	wait, total := intField(t, lineB, "admission_ms"), intField(t, lineB, "total_ms")
	if wait < hold.Milliseconds() || total < wait {
		t.Fatalf("caller B was held %v: admission_ms=%d total_ms=%d", hold, wait, total)
	}
	// A held its slot in the hook, which counts as work, not admission wait.
	if waitA, totalA := intField(t, lineA, "admission_ms"), intField(t, lineA, "total_ms"); waitA >= hold.Milliseconds() || totalA < hold.Milliseconds() {
		t.Fatalf("caller A line: %q", lineA)
	}
}

// T2: a request under the threshold prints no slow line.
func TestRequestTimingFastRequestPrintsNoLine(t *testing.T) {
	c := newClient(t)
	timing, lines := timed(c, RequestTimingConfig{})
	if code, _, _ := c.getAs("", "", "/v1/whoami", nil); code != 200 {
		t.Fatalf("whoami: %d", code)
	}
	if slow := lines.with(slowLinePrefix); len(slow) != 0 {
		t.Fatalf("fast request printed %q", slow)
	}
	timing.flush()
	minute := lines.with(minuteLinePrefix)
	if len(minute) != 1 || intField(t, minute[0], "requests") != 1 || intField(t, minute[0], "slow") != 0 {
		t.Fatalf("minute lines: %q", minute)
	}
}

// T3: the events long polls are never timed or counted.
func TestRequestTimingSkipsLongPolls(t *testing.T) {
	c := newClient(t)
	task := c.task("timing")
	timing, lines := timed(c, RequestTimingConfig{Threshold: time.Millisecond})
	for _, path := range []string{"/v1/tasks/" + task.ID + "/events?after=999999999&wait=300ms", "/v1/events?after=999999999&wait=300ms"} {
		start := time.Now()
		if code, _, _ := c.getAs("", "", path, nil); code != 200 {
			t.Fatalf("%s: %d", path, code)
		}
		if time.Since(start) < 250*time.Millisecond {
			t.Fatalf("%s did not wait", path)
		}
	}
	timing.flush()
	if got := lines.all(); len(got) != 0 {
		t.Fatalf("long polls printed %q", got)
	}
}

// T4: the minute line carries the store's pool and write-lock deltas, and a
// minute with no timed request prints nothing.
func TestRequestTimingMinuteLineCarriesPoolCounters(t *testing.T) {
	c := newClient(t)
	var mu sync.Mutex
	stats := store.PoolStats{WaitCount: 10, WaitDuration: time.Second, WriteLocks: 7, WriteLockWait: time.Second}
	read := func() store.PoolStats { mu.Lock(); defer mu.Unlock(); return stats }
	timing, lines := timed(c, RequestTimingConfig{Stats: read})
	mu.Lock()
	stats.WaitCount += 5
	stats.WaitDuration += 2 * time.Second
	stats.InUse = 1
	stats.WriteLocks += 3
	stats.WriteLockWait += 1500 * time.Millisecond
	mu.Unlock()
	if code, _, _ := c.getAs("", "", "/v1/whoami", nil); code != 200 {
		t.Fatalf("whoami: %d", code)
	}
	timing.flush()
	minute := lines.with(minuteLinePrefix)
	if len(minute) != 1 {
		t.Fatalf("minute lines: %q", lines.all())
	}
	line := minute[0]
	for key, want := range map[string]int64{"requests": 1, "slow": 0, "inflight": 0, "waiting": 0, "db_waits": 5,
		"db_wait_ms": 2000, "db_in_use": 1, "write_locks": 3, "write_lock_wait_ms": 1500, "slow_held_back": 0} {
		if got := intField(t, line, key); got != want {
			t.Fatalf("%s=%d, want %d: %q", key, got, want, line)
		}
	}
	if field(t, line, "slowest_route") != `"GET /v1/whoami"` {
		t.Fatalf("slowest route: %q", line)
	}
	intField(t, line, "total_ms")
	intField(t, line, "admission_ms")
	intField(t, line, "slowest_ms")

	timing.flush()
	if got := lines.with(minuteLinePrefix); len(got) != 1 {
		t.Fatalf("an idle minute printed %q", got[1:])
	}
}

// The real store counts write-lock acquisitions.
func TestStorePoolStatsCountWriteLocks(t *testing.T) {
	c := newClient(t)
	before := c.st.PoolStats()
	c.task("locks")
	after := c.st.PoolStats()
	if after.WriteLocks <= before.WriteLocks || after.WriteLockWait < before.WriteLockWait {
		t.Fatalf("write locks before %+v after %+v", before, after)
	}
}

// T5: slow lines are capped per minute and the next line counts those held
// back, as does the minute line.
func TestRequestTimingRateLimitsSlowLines(t *testing.T) {
	c := newClient(t)
	clock := &fakeClock{now: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	timing, lines := timed(c, RequestTimingConfig{Threshold: time.Nanosecond, LinesPerMinute: 2, Now: clock.read})
	whoami := func() {
		t.Helper()
		if code, _, _ := c.getAs("", "", "/v1/whoami", nil); code != 200 {
			t.Fatalf("whoami: %d", code)
		}
	}
	for i := 0; i < 5; i++ {
		whoami()
	}
	slow := lines.with(slowLinePrefix)
	if len(slow) != 2 {
		t.Fatalf("want 2 slow lines in the first minute, got %q", slow)
	}
	for _, line := range slow {
		if strings.Contains(line, "held_back=") {
			t.Fatalf("nothing was held back yet: %q", line)
		}
	}
	clock.advance(2 * time.Minute)
	whoami()
	slow = lines.with(slowLinePrefix)
	if len(slow) != 3 || intField(t, slow[2], "held_back") != 3 {
		t.Fatalf("want a third line counting 3 held back, got %q", slow)
	}
	timing.flush()
	minute := lines.with(minuteLinePrefix)
	if len(minute) != 1 || intField(t, minute[0], "slow") != 6 || intField(t, minute[0], "slow_held_back") != 3 {
		t.Fatalf("minute lines: %q", minute)
	}
	whoami()
	timing.flush()
	minute = lines.with(minuteLinePrefix)
	if len(minute) != 2 || intField(t, minute[1], "slow_held_back") != 0 {
		t.Fatalf("held-back count should reset each minute: %q", minute)
	}
	if slow = lines.with(slowLinePrefix); strings.Contains(slow[len(slow)-1], "held_back=") {
		t.Fatalf("held-back count should reset after it is printed: %q", slow)
	}
}

// T6: no line carries a path value, query, header, method text, caller key,
// agent or run ID.
func TestRequestTimingLinesCarryNoRequestData(t *testing.T) {
	c := newClient(t)
	timing, lines := timed(c, RequestTimingConfig{Threshold: time.Nanosecond})
	agent := api.NewID("agt")
	send := func(method, path string) {
		t.Helper()
		req, err := http.NewRequest(method, c.srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(agentHeader, agent)
		req.Header.Set(runHeader, "run_PLANTEDRUN")
		req.Header.Set("X-Planted", "PLANTEDHEADER")
		req.Header.Set("Authorization", "Bearer PLANTEDTOKEN")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	send("GET", "/v1/tasks/PLANTEDPATH/agents?q=PLANTEDQUERY")
	send("PLANTEDMETHOD", "/v1/PLANTEDUNMATCHED?q=PLANTEDQUERY")
	timing.flush()
	got := lines.all()
	slow := lines.with(slowLinePrefix)
	if len(slow) != 2 || len(lines.with(minuteLinePrefix)) != 1 {
		t.Fatalf("want two slow lines and a minute line, got %q", got)
	}
	if field(t, slow[0], "route") != `"GET /v1/tasks/{id}/agents"` || field(t, slow[1], "method") != "OTHER" ||
		field(t, slow[1], "route") != "unmatched" {
		t.Fatalf("slow lines: %q", slow)
	}
	for _, line := range got {
		for _, planted := range []string{"PLANTED", agent, "agent:", "node:", "Bearer"} {
			if strings.Contains(line, planted) {
				t.Fatalf("line leaks %q: %q", planted, line)
			}
		}
	}
}

// T7: the threshold variable is a positive Go duration, 1 s when unset.
func TestRequestTimingThreshold(t *testing.T) {
	read := func(value string) func(string) string {
		return func(key string) string {
			if key != SlowRequestThresholdEnv {
				t.Fatalf("read %s", key)
			}
			return value
		}
	}
	for value, want := range map[string]time.Duration{"": time.Second, "250ms": 250 * time.Millisecond, "3s": 3 * time.Second} {
		if got, err := RequestTimingThreshold(read(value)); err != nil || got != want {
			t.Fatalf("%q: %v %v, want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"0", "-1s", "x", "5"} {
		if _, err := RequestTimingThreshold(read(value)); err == nil {
			t.Fatalf("%q should be refused", value)
		}
	}
}
