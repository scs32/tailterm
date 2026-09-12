package main

import (
	"github.com/scs32/tailterm/hub/internal/monitor"
	"testing"
	"time"
)

func TestScheduleMonitorEnvironment(t *testing.T) {
	keys := []string{"TAILTERM_SCHEDULE_MONITOR_INTERVAL", "TAILTERM_SCHEDULE_MONITOR_STALL_AFTER", "TAILTERM_SCHEDULE_MONITOR_WORKER_SILENCE", "TAILTERM_SCHEDULE_MONITOR_INITIAL_BACKOFF", "TAILTERM_SCHEDULE_MONITOR_MAX_BACKOFF", "TAILTERM_SCHEDULE_MONITOR_NOTICE_RETENTION"}
	for _, key := range keys {
		t.Setenv(key, "")
	}
	c, err := scheduleMonitorConfig()
	if err != nil || c != monitor.DefaultConfig() {
		t.Fatalf("defaults %+v %v", c, err)
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			for _, invalid := range []string{"nope", "0s", "-1s"} {
				t.Setenv(key, invalid)
				if _, err := scheduleMonitorConfig(); err == nil {
					t.Fatalf("accepted %q", invalid)
				}
			}
			t.Setenv(key, "10m")
			if key == "TAILTERM_SCHEDULE_MONITOR_INITIAL_BACKOFF" {
				t.Setenv("TAILTERM_SCHEDULE_MONITOR_MAX_BACKOFF", "10m")
			}
			c, err := scheduleMonitorConfig()
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]time.Duration{keys[0]: c.Interval, keys[1]: c.StallAfter, keys[2]: c.WorkerSilence, keys[3]: c.InitialBackoff, keys[4]: c.MaxBackoff, keys[5]: c.NoticeRetention}
			if values[key] != 10*time.Minute {
				t.Fatal("override ignored")
			}
		})
	}
	t.Setenv(keys[3], "2h")
	t.Setenv(keys[4], "1h")
	if _, err := scheduleMonitorConfig(); err == nil {
		t.Fatal("inverted backoff accepted")
	}
}
