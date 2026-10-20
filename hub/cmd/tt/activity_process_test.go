package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/store"
)

func TestExactRuntimeAbsenceWithSurvivingTmux(t *testing.T) {
	state := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", state)
	t.Setenv("TT_TMUX_SOCKET", "")
	b := runtimeBinding{Hub: "https://synthetic.invalid", Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Session: "fake-session"}
	a := api.Agent{TaskID: b.Task, ID: b.Agent, RunID: b.Run, Session: b.Session, Status: api.AgentRunning, Online: true}
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	receipt := runtimeProcessReceipt{Hub: b.Hub, Task: b.Task, Agent: b.Agent, Run: b.Run, Session: b.Session, SessionID: "$1", SessionCreated: "123", PID: 4321, Started: "synthetic-start"}
	path := runtimeProcessPath(b.Hub, b.Agent, b.Run)
	if err := writePrivateJSON(path, receipt); err != nil {
		t.Fatal(err)
	}
	tmuxProbe := func(string) (bool, error) { return true, nil }
	sessionProbe := func(r runtimeProcessReceipt) error {
		if r.SessionID != "$1" || r.SessionCreated != "123" {
			return errors.New("session reused")
		}
		return nil
	}
	missingPID := func(pid int, created string) (bool, error) {
		if pid != 4321 || created != "synthetic-start" {
			t.Fatalf("wrong identity %d %q", pid, created)
		}
		return false, nil
	}
	tmux, process, err := probeExactRuntimeProcess(b, a, tmuxProbe, sessionProbe, missingPID)
	if err != nil || !tmux || process {
		t.Fatalf("exact child absence with tmux alive: %v %v %v", tmux, process, err)
	}
	c := activityCursor{SeenTurn: true, LastEventAt: now}
	first := activityState(&c, a, 0, tmux, process, err, now, activityDefaults())
	second := activityState(&c, a, 0, tmux, process, err, now.Add(16*time.Second), activityDefaults())
	if first.State != "unknown" || second.State != "crashed" {
		t.Fatalf("two-probe crash: %+v %+v", first, second)
	}

	// The wrapper's exact exit receipt is sufficient even if a PID is reused.
	receipt.ExitedAt = now
	if err := writePrivateJSON(path, receipt); err != nil {
		t.Fatal(err)
	}
	tmux, process, err = probeExactRuntimeProcess(b, a, tmuxProbe, sessionProbe, func(int, string) (bool, error) { t.Fatal("exited receipt probed reused pid"); return false, nil })
	if err != nil || !tmux || process {
		t.Fatalf("exact exit receipt: %v %v %v", tmux, process, err)
	}
}

func TestRuntimeIdentityAndProbeUncertaintyNeverCrash(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Setenv("TT_TMUX_SOCKET", "")
	b := runtimeBinding{Hub: "https://synthetic.invalid", Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Session: "fake-session"}
	a := api.Agent{TaskID: b.Task, ID: b.Agent, RunID: b.Run, Session: b.Session, Status: api.AgentRunning, Online: false}
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	receipt := runtimeProcessReceipt{Hub: b.Hub, Task: b.Task, Agent: b.Agent, Run: b.Run, Session: b.Session, SessionID: "$1", SessionCreated: "123", PID: 4321, Started: "synthetic-start"}
	path := runtimeProcessPath(b.Hub, b.Agent, b.Run)
	if err := writePrivateJSON(path, receipt); err != nil {
		t.Fatal(err)
	}
	tmuxProbe := func(string) (bool, error) { return true, nil }
	sessionProbe := func(r runtimeProcessReceipt) error {
		if r.SessionID != "$1" || r.SessionCreated != "123" {
			return errors.New("session reused")
		}
		return nil
	}
	checkUnknown := func(name string, agent api.Agent, tmux func(string) (bool, error), pid runtimePIDProbe) {
		t.Helper()
		alive, process, err := probeExactRuntimeProcess(b, agent, tmux, sessionProbe, pid)
		c := activityCursor{SeenTurn: true, LastEventAt: now, MissingSince: now.Add(-time.Minute)}
		got := activityState(&c, agent, 0, alive, process, err, now, activityDefaults())
		if err == nil || got.State != "unknown" || !c.MissingSince.IsZero() {
			t.Fatalf("%s false crash: %+v %v", name, got, err)
		}
	}
	checkUnknown("reused PID", a, tmuxProbe, func(int, string) (bool, error) { return false, errors.New("creation identity changed") })
	checkUnknown("process probe failure", a, tmuxProbe, func(int, string) (bool, error) { return false, errors.New("permission") })
	checkUnknown("tmux probe failure", a, func(string) (bool, error) { return false, errors.New("socket") }, func(int, string) (bool, error) { t.Fatal("unexpected pid probe"); return false, nil })
	mismatched := a
	mismatched.RunID = "run_fedcba9876543210"
	checkUnknown("mismatched run", mismatched, tmuxProbe, func(int, string) (bool, error) { t.Fatal("unexpected pid probe"); return false, nil })
	mismatched = a
	mismatched.Session = "reused-session"
	checkUnknown("reused session", mismatched, tmuxProbe, func(int, string) (bool, error) { t.Fatal("unexpected pid probe"); return false, nil })
	if tmux, process, err := probeExactRuntimeProcess(b, a, tmuxProbe, func(runtimeProcessReceipt) error { return errors.New("tmux session creation changed") }, func(int, string) (bool, error) { t.Fatal("reused session reached pid probe"); return false, nil }); err == nil || !tmux || process {
		t.Fatalf("session creation reuse: %v %v %v", tmux, process, err)
	}
	t.Setenv("TT_TMUX_SOCKET", "wrong-socket")
	checkUnknown("wrong tmux socket", a, tmuxProbe, func(int, string) (bool, error) { t.Fatal("wrong socket reached pid probe"); return false, nil })
	t.Setenv("TT_TMUX_SOCKET", "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	checkUnknown("missing identity", a, tmuxProbe, func(int, string) (bool, error) { t.Fatal("unexpected pid probe"); return false, nil })
	if err := writePrivateJSON(path, receipt); err != nil {
		t.Fatal(err)
	}
	tmux, process, err := probeExactRuntimeProcess(b, a, tmuxProbe, sessionProbe, func(int, string) (bool, error) { return true, nil })
	if err != nil || !tmux || !process {
		t.Fatalf("live runtime lost to heartbeat gap: %v %v %v", tmux, process, err)
	}
	c := activityCursor{SeenTurn: true, LastEventAt: now}
	if got := activityState(&c, a, 0, tmux, process, err, now.Add(16*time.Second), activityDefaults()); got.State != "working" {
		t.Fatalf("live heartbeat gap: %+v", got)
	}
}

func TestWrapperRecordsExactProcessAndExit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
	t.Setenv(spawn.EnvSession, "tt-handler-0123456789abcdef")
	t.Setenv("TT_TMUX_SOCKET", "synthetic-activity-process")
	ps := filepath.Join(home, "ps")
	if err := os.WriteFile(ps, []byte("#!/bin/sh\nprintf 'synthetic-start\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	tmux := filepath.Join(home, "tmux")
	line := `["$1","123","tt-handler-0123456789abcdef","https://synthetic.invalid","tsk_0123456789abcdef","agt_0123456789abcdef","run_0123456789abcdef"]`
	if err := os.WriteFile(tmux, []byte("#!/bin/sh\nprintf '%s\\n' '"+line+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home+":"+os.Getenv("PATH"))
	e := env{hub: "https://synthetic.invalid", task: "tsk_0123456789abcdef", agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
	recordRuntimeProcess(e, 4321)
	path := runtimeProcessPath(e.hub, e.agent, e.runID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt runtimeProcessReceipt
	if err = json.Unmarshal(data, &receipt); err != nil || receipt.PID != 4321 || receipt.Started != "synthetic-start" || receipt.Session != "tt-handler-0123456789abcdef" || receipt.Socket != "synthetic-activity-process" || receipt.SessionID != "$1" || receipt.SessionCreated != "123" {
		t.Fatalf("start receipt: %+v %v", receipt, err)
	}
	if err := nativeRuntimeSessionProbe(receipt); err != nil {
		t.Fatalf("exact fake tmux session: %v", err)
	}
	reused := receipt
	reused.SessionCreated = "124"
	if err := nativeRuntimeSessionProbe(reused); err == nil {
		t.Fatal("accepted reused tmux session")
	}
	other := e
	other.runID = "run_00000000000000cc"
	if _, ok := recordRuntimeExit(other); ok {
		t.Fatal("recorded the exit of another run")
	}
	saved, ok := recordRuntimeExit(e)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &receipt); err != nil || receipt.ExitedAt.IsZero() || !ok || saved != receipt {
		t.Fatalf("exit receipt: %+v %v returned %+v %t", receipt, err, saved, ok)
	}
}

// p1: the wrapper's receipt records its session's single pane process, that
// process's start identity and the login shell spawn.Wrap leaves in the pane,
// and the exit keeps them. Any other pane answer records no pane and leaves
// the session identity as read.
func TestWrapperRecordsPaneProcess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
	t.Setenv(spawn.EnvSession, "tt-handler-0123456789abcdef")
	t.Setenv("TT_TMUX_SOCKET", "synthetic-activity-pane")
	t.Setenv("SHELL", "/bin/synthetic-shell")
	// ps -o lstart= -p PID prints a start identity naming the PID.
	if err := os.WriteFile(filepath.Join(home, "ps"), []byte("#!/bin/sh\nprintf 'start-%s\\n' \"$4\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(home, "panes.log")
	line := `["$1","123","tt-handler-0123456789abcdef","https://synthetic.invalid","tsk_0123456789abcdef","agt_0123456789abcdef","run_0123456789abcdef"]`
	fake := "#!/bin/sh\ncase \" $* \" in *\" list-panes \"*)\n  printf '%s\\n' \"$*\" >> '" + log + "'\n  if [ \"$FAKE_PANES\" = fail ]; then exit 1; fi\n  for p in $FAKE_PANES; do printf '%s\\n' \"$p\"; done\n  exit 0;;\nesac\nprintf '%s\\n' '" + line + "'\n"
	if err := os.WriteFile(filepath.Join(home, "tmux"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home+":"+os.Getenv("PATH"))
	e := env{hub: "https://synthetic.invalid", task: "tsk_0123456789abcdef", agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
	path := runtimeProcessPath(e.hub, e.agent, e.runID)
	read := func() runtimeProcessReceipt {
		t.Helper()
		var receipt runtimeProcessReceipt
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &receipt) != nil {
			t.Fatalf("receipt unreadable: %v", err)
		}
		return receipt
	}
	record := func(panes string) runtimeProcessReceipt {
		t.Helper()
		t.Setenv("FAKE_PANES", panes)
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		recordRuntimeProcess(e, 4321)
		return read()
	}
	want := runtimeProcessReceipt{Hub: e.hub, Task: e.task, Agent: e.agent, Run: e.runID, Session: "tt-handler-0123456789abcdef", Socket: "synthetic-activity-pane", PID: 4321, Started: "start-4321",
		PanePID: 4200, PaneStarted: "start-4200", Shell: "/bin/synthetic-shell", SessionID: "$1", SessionCreated: "123"}
	if got := record("4200"); got != want {
		t.Fatalf("one pane:\n got %+v\nwant %+v", got, want)
	}
	if logged, err := os.ReadFile(log); err != nil || string(logged) != "-L synthetic-activity-pane list-panes -s -t $1 -F #{pane_pid}\n" {
		t.Fatalf("pane read: %q %v", logged, err)
	}
	recordRuntimeExit(e)
	exited := read()
	if exited.ExitedAt.IsZero() {
		t.Fatalf("exit receipt: %+v", exited)
	}
	exited.ExitedAt = time.Time{}
	if exited != want {
		t.Fatalf("the exit changed the receipt:\n got %+v\nwant %+v", exited, want)
	}

	noPane := want
	noPane.PanePID, noPane.PaneStarted = 0, ""
	for name, panes := range map[string]string{"two panes": "4200 4201", "no pane": "", "failing pane read": "fail", "unreadable pane": "pane"} {
		if got := record(panes); got != noPane {
			t.Fatalf("%s:\n got %+v\nwant %+v", name, got, noPane)
		}
	}
	t.Setenv("SHELL", "")
	want.Shell = "/bin/sh"
	if got := record("4200"); got != want {
		t.Fatalf("no SHELL:\n got %+v\nwant %+v", got, want)
	}
}

// A failed read of the tmux session identity is retried once; only two failed
// reads save a receipt without it, and that receipt says so.
func TestWrapperRetriesSessionIdentityReadOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
	t.Setenv(spawn.EnvSession, "tt-handler-0123456789abcdef")
	t.Setenv("TT_TMUX_SOCKET", "synthetic-activity-retry")
	if err := os.WriteFile(filepath.Join(home, "ps"), []byte("#!/bin/sh\nprintf 'synthetic-start\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(home, "calls")
	line := `["$1","123","tt-handler-0123456789abcdef","https://synthetic.invalid","tsk_0123456789abcdef","agt_0123456789abcdef","run_0123456789abcdef"]`
	// The fake tmux fails its first FAILS calls, then prints the session.
	// A pane read is not a session read: it fails and is not counted.
	fake := "#!/bin/sh\ncase \" $* \" in *\" list-panes \"*) exit 1;; esac\nprintf x >> '" + calls + "'\nif [ \"$(wc -c < '" + calls + "')\" -le \"$FAKE_TMUX_FAILS\" ]; then echo 'synthetic tmux failure' >&2; exit 1; fi\nprintf '%s\\n' '" + line + "'\n"
	if err := os.WriteFile(filepath.Join(home, "tmux"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home+":"+os.Getenv("PATH"))
	e := env{hub: "https://synthetic.invalid", task: "tsk_0123456789abcdef", agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
	record := func(fails string) (runtimeProcessReceipt, int) {
		t.Helper()
		t.Setenv("FAKE_TMUX_FAILS", fails)
		if err := os.RemoveAll(calls); err != nil {
			t.Fatal(err)
		}
		recordRuntimeProcess(e, 4321)
		var receipt runtimeProcessReceipt
		data, err := os.ReadFile(runtimeProcessPath(e.hub, e.agent, e.runID))
		if err != nil || json.Unmarshal(data, &receipt) != nil {
			t.Fatalf("receipt after %s failed reads: %v", fails, err)
		}
		made, _ := os.ReadFile(calls)
		return receipt, len(made)
	}
	if receipt, made := record("0"); made != 1 || receipt.SessionID != "$1" || receipt.SessionCreated != "123" || receipt.SessionError != "" {
		t.Fatalf("first read succeeded: %d reads %+v", made, receipt)
	}
	if receipt, made := record("1"); made != 2 || receipt.SessionID != "$1" || receipt.SessionCreated != "123" || receipt.SessionError != "" {
		t.Fatalf("one failed read then success: %d reads %+v", made, receipt)
	}
	receipt, made := record("2")
	if made != 2 || receipt.SessionID != "" || receipt.SessionCreated != "" || !strings.Contains(receipt.SessionError, "unread after 2 attempts") {
		t.Fatalf("two failed reads: %d reads %+v", made, receipt)
	}
	if receipt.PID != 4321 || receipt.Started != "synthetic-start" || nativeRuntimeSessionProbe(receipt) == nil {
		t.Fatalf("receipt without session identity: %+v", receipt)
	}
}

// A tmux that answers only after the first read's deadline is read on the
// retry. One that outlasts both deadlines leaves a receipt without a session
// identity, unless a complete adopted receipt for the same run and session was
// already saved: its session identity and pane fields are kept.
func TestWrapperSessionReadDeadlines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
	t.Setenv(spawn.EnvSession, "tt-handler-0123456789abcdef")
	t.Setenv("TT_TMUX_SOCKET", "synthetic-activity-deadline")
	t.Setenv("SHELL", "/bin/synthetic-shell")
	if err := os.WriteFile(filepath.Join(home, "ps"), []byte("#!/bin/sh\nprintf 'synthetic-start\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	line := `["$1","123","tt-handler-0123456789abcdef","https://synthetic.invalid","tsk_0123456789abcdef","agt_0123456789abcdef","run_0123456789abcdef"]`
	// The slow fake cannot answer before half a second, however fast the host
	// is, so a shorter deadline is always missed; the other mode fails at once.
	// The sleep keeps no pipe open, so a killed read returns at its deadline.
	fake := "#!/bin/sh\ncase \" $* \" in *\" list-panes \"*) exit 1;; esac\nif [ \"$FAKE_TMUX_MODE\" = fail ]; then exit 1; fi\nsleep 0.5 >/dev/null 2>&1\nprintf '%s\\n' '" + line + "'\n"
	if err := os.WriteFile(filepath.Join(home, "tmux"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home+":"+os.Getenv("PATH"))
	product := runtimeSessionReadTimeouts
	t.Cleanup(func() { runtimeSessionReadTimeouts = product })
	e := env{hub: "https://synthetic.invalid", task: "tsk_0123456789abcdef", agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
	path := runtimeProcessPath(e.hub, e.agent, e.runID)
	// What the wrapper saves with nothing to keep, but for SessionError.
	bare := runtimeProcessReceipt{Hub: e.hub, Task: e.task, Agent: e.agent, Run: e.runID, Session: "tt-handler-0123456789abcdef", Socket: "synthetic-activity-deadline", PID: 4321, Started: "synthetic-start",
		Shell: "/bin/synthetic-shell"}
	adopted := bare
	adopted.Shell = ""
	adopted.PID, adopted.Started, adopted.PanePID, adopted.PaneStarted = 99, "adopted-start", 98, "adopted-pane-start"
	adopted.SessionID, adopted.SessionCreated = "$7", "777"
	record := func(name, mode string, first, second time.Duration, existing any) runtimeProcessReceipt {
		t.Helper()
		t.Setenv("FAKE_TMUX_MODE", mode)
		runtimeSessionReadTimeouts = [2]time.Duration{first, second}
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		switch v := existing.(type) {
		case string:
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(v), 0600); err != nil {
				t.Fatal(err)
			}
		case runtimeProcessReceipt:
			if err := writePrivateJSON(path, v); err != nil {
				t.Fatal(err)
			}
		}
		recordRuntimeProcess(e, 4321)
		var receipt runtimeProcessReceipt
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &receipt) != nil {
			t.Fatalf("%s: receipt unreadable: %v", name, err)
		}
		return receipt
	}
	unread := func(name string, receipt runtimeProcessReceipt) runtimeProcessReceipt {
		t.Helper()
		if !strings.Contains(receipt.SessionError, "unread after 2 attempts") {
			t.Fatalf("%s: session error: %+v", name, receipt)
		}
		receipt.SessionError = ""
		return receipt
	}
	read := bare
	read.SessionID, read.SessionCreated = "$1", "123"
	kept := bare
	kept.SessionID, kept.SessionCreated = "$7", "777"
	kept.PanePID, kept.PaneStarted = 98, "adopted-pane-start"

	// The first read misses its deadline and the retry answers. What tmux
	// reported replaces an adopted identity, as any successful read does.
	if got := record("retry answers", "slow", 50*time.Millisecond, 30*time.Second, nil); got != read {
		t.Fatalf("first deadline missed, retry answered: %+v", got)
	}
	if got := record("retry answers over adopted", "slow", 50*time.Millisecond, 30*time.Second, adopted); got != read {
		t.Fatalf("first deadline missed, retry answered over an adopted receipt: %+v", got)
	}

	// Both reads miss their deadlines.
	got := record("nothing to keep", "slow", 50*time.Millisecond, 100*time.Millisecond, nil)
	if unread("nothing to keep", got) != bare || nativeRuntimeSessionProbe(got) == nil {
		t.Fatalf("both deadlines missed with no receipt: %+v", got)
	}
	got = record("keep", "slow", 50*time.Millisecond, 100*time.Millisecond, adopted)
	if unread("keep", got) != kept {
		t.Fatalf("both deadlines missed over an adopted receipt: %+v", got)
	}

	// Both reads fail at once: every receipt that is not a complete adopted
	// one for this run and session leaves the wrapper's receipt as it was.
	change := func(edit func(*runtimeProcessReceipt)) runtimeProcessReceipt {
		r := adopted
		edit(&r)
		return r
	}
	for name, existing := range map[string]any{
		"unreadable":         "{invalid",
		"other hub":          change(func(r *runtimeProcessReceipt) { r.Hub = "https://other.invalid" }),
		"other task":         change(func(r *runtimeProcessReceipt) { r.Task = "tsk_fedcba9876543210" }),
		"other agent":        change(func(r *runtimeProcessReceipt) { r.Agent = "agt_fedcba9876543210" }),
		"other run":          change(func(r *runtimeProcessReceipt) { r.Run = "run_fedcba9876543210" }),
		"other session":      change(func(r *runtimeProcessReceipt) { r.Session = "tt-handler-fedcba9876543210" }),
		"other socket":       change(func(r *runtimeProcessReceipt) { r.Socket = "synthetic-other" }),
		"no session id":      change(func(r *runtimeProcessReceipt) { r.SessionID = "" }),
		"no session created": change(func(r *runtimeProcessReceipt) { r.SessionCreated = "" }),
		"wrapper's own":      change(func(r *runtimeProcessReceipt) { r.PanePID, r.PaneStarted = 0, "" }),
		"wrapper's own pane": change(func(r *runtimeProcessReceipt) { r.Shell = "/bin/synthetic-shell" }),
		"no pane pid":        change(func(r *runtimeProcessReceipt) { r.PanePID = 0 }),
		"no pane started":    change(func(r *runtimeProcessReceipt) { r.PaneStarted = "" }),
	} {
		got := record(name, "fail", time.Second, time.Second, existing)
		if unread(name, got) != bare {
			t.Fatalf("%s: kept an identity that was not this run's: %+v", name, got)
		}
	}
	got = record("keep after failed reads", "fail", time.Second, time.Second, adopted)
	if unread("keep after failed reads", got) != kept {
		t.Fatalf("both reads failed over an adopted receipt: %+v", got)
	}
}

// The fake commands expose precisely the columns used by production discovery.
func TestActivityHandlerRuntimeAdoption(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
	t.Setenv("TT_TMUX_SOCKET", "test-handler")
	t.Setenv("PATH", home+":"+os.Getenv("PATH"))
	b := runtimeBinding{Hub: "https://synthetic.invalid", Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Session: "tt-handler-0123456789abcdef", Runtime: "codex"}
	a := api.Agent{TaskID: b.Task, ID: b.Agent, RunID: b.Run, Session: b.Session, Runtime: b.Runtime, Status: api.AgentRunning}
	pane := `["$1","123","tt-handler-0123456789abcdef","https://synthetic.invalid","tsk_0123456789abcdef","agt_0123456789abcdef","run_0123456789abcdef","100"]`
	processes := "100 1 Fri Sep 25 20:00:00 2026 /bin/zsh\n101 100 Fri Sep 25 20:00:01 2026 /bin/tt\n102 101 Fri Sep 25 20:00:02 2026 /bin/codex\n"
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, name), []byte("#!/bin/sh\ncat <<'DATA'\n"+body+"\nDATA\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("tmux", pane)
	write("ps", processes)
	receipt, err := nativeRuntimeDiscovery(context.Background(), b, a)
	if err != nil || receipt.PID != 102 || receipt.PanePID != 100 || receipt.Socket != "test-handler" {
		t.Fatalf("handler discovery %+v %v", receipt, err)
	}
	tmux := func(string) (bool, error) { return true, nil }
	session := func(r runtimeProcessReceipt) error {
		if r.SessionID != "$1" {
			return errors.New("session")
		}
		return nil
	}
	alive := true
	pid := func(p int, s string) (bool, error) {
		if p != 102 || s != "Fri Sep 25 20:00:02 2026" {
			t.Fatalf("unexpected process %d %q", p, s)
		}
		return alive, nil
	}
	path := runtimeProcessPath(b.Hub, b.Agent, b.Run)
	if _, _, err := probeExactRuntimeProcess(b, a, tmux, session, pid); err == nil {
		t.Fatal("baseline missing receipt must be unknown")
	}
	for _, mode := range []string{"adopted", "fresh"} {
		if mode == "fresh" {
			if err := writePrivateJSON(path, receipt); err != nil {
				t.Fatal(err)
			}
		}
		tm, pr, err := probeRuntimeWithDiscovery(b, a, tmux, session, pid, nativeRuntimeDiscovery)
		if err != nil || !tm || !pr {
			t.Fatalf("%s handler probe %v %v %v", mode, tm, pr, err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private adopted receipt: %v %v", info, err)
		}
		now := time.Date(2026, 9, 25, 20, 0, 5, 0, time.UTC)
		c := activityCursor{SeenTurn: true, LastEventAt: now}
		if got := activityState(&c, a, 0, tm, pr, err, now, activityDefaults()); got.State != "working" {
			t.Fatal(got)
		}
		c.TurnComplete = true
		if got := activityState(&c, a, 0, tm, pr, err, now, activityDefaults()); got.State != "idle" {
			t.Fatal(got)
		}
		c.TurnComplete = false
		c.Pending = map[string]pendingActivityCall{"x": {Name: "exec_command", Since: now.Add(-11 * time.Minute)}}
		if got := activityState(&c, a, 0, tm, pr, err, now, activityDefaults()); got.State != "hung_tool" {
			t.Fatal(got)
		}
	}
	alive = false
	tm, pr, err := probeRuntimeWithDiscovery(b, a, tmux, session, pid, func(context.Context, runtimeBinding, api.Agent) (runtimeProcessReceipt, error) {
		t.Fatal("saved receipt rediscovered")
		return receipt, nil
	})
	now := time.Date(2026, 9, 25, 20, 0, 5, 0, time.UTC)
	c := activityCursor{SeenTurn: true, LastEventAt: now}
	first := activityState(&c, a, 0, tm, pr, err, now, activityDefaults())
	second := activityState(&c, a, 0, tm, pr, err, now.Add(16*time.Second), activityDefaults())
	if first.State != "unknown" || second.State != "crashed" {
		t.Fatalf("handler crash %v %v", first, second)
	}
}

func TestActivityRuntimeDiscoveryRejections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
	t.Setenv("TT_TMUX_SOCKET", "fake")
	t.Setenv("PATH", home+":"+os.Getenv("PATH"))
	b := runtimeBinding{Hub: "https://synthetic.invalid", Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Session: "tt-handler-0123456789abcdef", Runtime: "codex"}
	a := api.Agent{TaskID: b.Task, ID: b.Agent, RunID: b.Run, Session: b.Session, Runtime: b.Runtime}
	pane := `["$1","123","tt-handler-0123456789abcdef","https://synthetic.invalid","tsk_0123456789abcdef","agt_0123456789abcdef","run_0123456789abcdef","100"]`
	base := "100 1 Fri Sep 25 20:00:00 2026 /bin/zsh\n101 100 Fri Sep 25 20:00:01 2026 /bin/codex\n"
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, name), []byte("#!/bin/sh\ncat <<'DATA'\n"+body+"\nDATA\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	deep := "100 1 Fri Sep 25 20:00:00 2026 zsh\n"
	for i := 101; i < 140; i++ {
		deep += fmt.Sprintf("%d %d Fri Sep 25 20:00:01 2026 tt\n", i, i-1)
	}
	deep += "140 139 Fri Sep 25 20:00:02 2026 codex\n"
	for _, tc := range []struct{ name, pane, ps string }{
		{"ambiguous panes", pane + "\n" + pane, base},
		{"wrong run", strings.Replace(pane, b.Run, "run_fedcba9876543210", 1), base},
		{"wrong hub", strings.Replace(pane, b.Hub, "https://other.invalid", 1), base},
		{"wrong session", strings.Replace(pane, b.Session, "reused", 1), base},
		{"no pane", pane, "101 1 Fri Sep 25 20:00:01 2026 codex"},
		{"shell only", pane, "100 1 Fri Sep 25 20:00:00 2026 zsh"},
		{"ambiguous runtimes", pane, base + "102 100 Fri Sep 25 20:00:02 2026 codex\n"},
		{"reused parent", pane, strings.Replace(base, "20:00:00", "20:00:05", 1)},
		{"reused session creation", strings.Replace(pane, `"123"`, `"9999999999"`, 1), base},
		{"depth budget", pane, deep},
		{"output budget", pane, strings.Repeat(base, 10000)},
		{"malformed process", pane, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write("tmux", tc.pane)
			write("ps", tc.ps)
			if _, err := nativeRuntimeDiscovery(context.Background(), b, a); err == nil {
				t.Fatal("unsafe discovery accepted")
			}
			if _, err := os.Stat(runtimeProcessPath(b.Hub, b.Agent, b.Run)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejection wrote receipt")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(home, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeRuntimeDiscovery(context.Background(), b, a); err == nil {
		t.Fatal("denial accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := nativeRuntimeDiscovery(ctx, b, a); err == nil {
		t.Fatal("exhausted time budget accepted")
	}
}

func TestActivityAdoptionNeverReplacesUncertainReceipts(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Setenv("TT_TMUX_SOCKET", "")
	b := runtimeBinding{Hub: "https://synthetic.invalid", Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Session: "tt-handler-0123456789abcdef"}
	a := api.Agent{TaskID: b.Task, ID: b.Agent, RunID: b.Run, Session: b.Session}
	receipt := runtimeProcessReceipt{Hub: b.Hub, Task: b.Task, Agent: b.Agent, Run: b.Run, Session: b.Session, SessionID: "$1", SessionCreated: "123", PID: 101, Started: "synthetic"}
	path := runtimeProcessPath(b.Hub, b.Agent, b.Run)
	tmux := func(string) (bool, error) { return true, nil }
	session := func(runtimeProcessReceipt) error { return nil }
	pid := func(int, string) (bool, error) { return true, nil }
	forbidden := func(context.Context, runtimeBinding, api.Agent) (runtimeProcessReceipt, error) {
		t.Fatal("existing uncertain receipt entered adoption")
		return receipt, nil
	}
	for _, mode := range []string{"malformed", "mismatched", "exited"} {
		r := receipt
		switch mode {
		case "malformed":
			if err := os.WriteFile(path, []byte("{invalid"), 0600); err != nil {
				t.Fatal(err)
			}
		case "mismatched":
			r.Run = "run_fedcba9876543210"
			writePrivateJSON(path, r)
		case "exited":
			r.ExitedAt = time.Now()
			writePrivateJSON(path, r)
		}
		before, _ := os.ReadFile(path)
		probeRuntimeWithDiscovery(b, a, tmux, session, pid, forbidden)
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Fatal("changed existing receipt")
		}
	}
	os.Remove(path)
	calls := 0
	changing := func(context.Context, runtimeBinding, api.Agent) (runtimeProcessReceipt, error) {
		calls++
		r := receipt
		if calls == 2 {
			r.SessionCreated = "124"
		}
		return r, nil
	}
	if _, _, err := probeRuntimeWithDiscovery(b, a, tmux, session, pid, changing); err == nil {
		t.Fatal("discovery race accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("race wrote receipt")
	}
	calls = 0
	wrapperRace := func(context.Context, runtimeBinding, api.Agent) (runtimeProcessReceipt, error) {
		calls++
		if calls == 2 {
			r := receipt
			r.ExitedAt = time.Now()
			if err := writePrivateJSON(path, r); err != nil {
				t.Fatal(err)
			}
		}
		return receipt, nil
	}
	if _, _, err := probeRuntimeWithDiscovery(b, a, tmux, session, pid, wrapperRace); err == nil {
		t.Fatal("concurrent wrapper receipt overwritten")
	}
	var saved runtimeProcessReceipt
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &saved)
	if saved.ExitedAt.IsZero() {
		t.Fatal("lost wrapper exit receipt")
	}
}

// exitReportHub is an isolated in-process hub with one running agent whose
// wrapper's receipt records its runtime's exit (wi_b01a7afa10979dd0). answer
// scripts the hub's reply to each exited post, counted from 0: 0 passes it
// to the hub, -1 drops the connection, and any other value is that status.
type exitReportHub struct {
	c      *api.Client
	e      env
	agent  api.Agent
	mu     sync.Mutex
	posts  int
	answer func(n int) int
}

func newExitReportHub(t *testing.T) *exitReportHub {
	t.Helper()
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(spawn.EnvSession, "tt-handler-exit-report-fixture")
	t.Setenv("TT_TMUX_SOCKET", "synthetic-exit-report")
	x := &exitReportHub{answer: func(int) int { return 0 }}
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	handler := server.New(st, func(r *http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/events") {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "unreadable fixture request", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var req api.PostEventRequest
			if json.Unmarshal(body, &req) == nil && req.Kind == api.EventExited {
				x.mu.Lock()
				answer := x.answer(x.posts)
				x.posts++
				x.mu.Unlock()
				switch {
				case answer < 0:
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				case answer > 0:
					http.Error(w, "fixture refusal", answer)
					return
				}
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	if x.c, err = api.NewClient(srv.URL, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, err := x.c.CreateTask(ctx, api.CreateTaskRequest{Name: "isolated exit report test"})
	if err != nil {
		t.Fatal(err)
	}
	if x.agent, err = x.c.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "db-handler", Host: "fixture", Session: "tt-handler-exit-report-fixture", Runtime: "generic"}); err != nil {
		t.Fatal(err)
	}
	if _, err = x.c.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: x.agent.ID, RunID: x.agent.RunID, Kind: api.EventStarted}); err != nil {
		t.Fatal(err)
	}
	x.e = env{hub: x.c.Base, task: task.ID, agent: x.agent.ID, runID: x.agent.RunID, session: "tt-handler-exit-report-fixture"}
	x.writeReceipt(t, func(*runtimeProcessReceipt) {})
	if _, ok := recordRuntimeExit(x.e); !ok {
		t.Fatal("the fixture receipt did not record the exit")
	}
	return x
}

// writeReceipt saves the receipt tt wrap writes at start, with the pane
// process 4200 (the wrapper) and its login shell, changed by change.
func (x *exitReportHub) writeReceipt(t *testing.T, change func(*runtimeProcessReceipt)) {
	t.Helper()
	r := runtimeProcessReceipt{Hub: x.e.hub, Task: x.e.task, Agent: x.e.agent, Run: x.e.runID, Session: "tt-handler-exit-report-fixture", Socket: "synthetic-exit-report", PID: 4242,
		Started: "start-4242", PanePID: 4200, PaneStarted: "Wed Oct  7 06:00:00 2026", Shell: "/bin/zsh", SessionID: "$7", SessionCreated: "1700000000"}
	change(&r)
	if err := writePrivateJSON(runtimeProcessPath(x.e.hub, x.e.agent, x.e.runID), r); err != nil {
		t.Fatal(err)
	}
}

func (x *exitReportHub) receipt(t *testing.T) runtimeProcessReceipt {
	t.Helper()
	var r runtimeProcessReceipt
	data, err := os.ReadFile(runtimeProcessPath(x.e.hub, x.e.agent, x.e.runID))
	if err != nil || json.Unmarshal(data, &r) != nil {
		t.Fatalf("receipt: %v", err)
	}
	return r
}

func (x *exitReportHub) exitPosts() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.posts
}

// newestExit is the text of the agent's newest started, heartbeat or exited event.
func (x *exitReportHub) newest(t *testing.T) api.Event {
	t.Helper()
	list, err := x.c.Events(context.Background(), x.e.task, 0, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var newest api.Event
	for _, ev := range list.Events {
		if ev.AgentID == x.e.agent && (ev.Kind == api.EventStarted || ev.Kind == api.EventHeartbeat || ev.Kind == api.EventExited) && ev.Seq > newest.Seq {
			newest = ev
		}
	}
	return newest
}

// idleShellTable is the host's process table with the wrapper 4200 as the
// pane process and only its login shell under it, started after the exit.
func idleShellTable(r runtimeProcessReceipt) map[int]discoveredProcess {
	started := r.ExitedAt.Add(time.Second).In(time.Local).Format("Mon Jan _2 15:04:05 2006")
	return map[int]discoveredProcess{1: {1, 0, "Sat Aug 22 19:33:24 2026", "/sbin/launchd"}, 4200: {4200, 1, "Wed Oct  7 06:00:00 2026", "tt"}, 4300: {4300, 4200, started, "-zsh"}}
}

// reporter is the production reporter with a synthetic process table and
// pane, and waits that record their duration and fire at once. stopAfter
// cancels ctx at that many waits (0 never).
type recordedWaits struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (w *recordedWaits) list() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.waits...)
}

func (x *exitReportHub) reporter(t *testing.T, cancel context.CancelFunc, stopAfter int, waits *recordedWaits) exitReporter {
	r := productionExitReporter(x.e)
	r.table = func(context.Context) (map[int]discoveredProcess, error) { return idleShellTable(x.receipt(t)), nil }
	r.self = func() int { return 4200 }
	r.after = func(d time.Duration) <-chan time.Time {
		waits.mu.Lock()
		waits.waits = append(waits.waits, d)
		n := len(waits.waits)
		waits.mu.Unlock()
		if stopAfter > 0 && n >= stopAfter {
			cancel()
		}
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	return r
}

// e2: the wrapper's failed exit report is retried in the background with
// the identical text until the hub accepts it, and the receipt records it.
func TestWrapperExitReportRetriesUntilAccepted(t *testing.T) {
	x := newExitReportHub(t)
	x.answer = func(n int) int {
		if n < 2 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits recordedWaits
	// The exit was recorded by the fixture; wrapperExited records it again.
	if !wrapperExited(ctx, x.e, 1, x.reporter(t, cancel, 0, &waits)) {
		t.Fatal("no retry started after the failed exit report")
	}
	deadline := time.Now().Add(10 * time.Second)
	for x.receipt(t).ExitReportedAt.IsZero() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	r := x.receipt(t)
	if r.ExitReportedAt.IsZero() || r.ExitReportStopped != "" || r.ExitReportedAt.Before(r.ExitedAt) || x.exitPosts() != 3 || !reflect.DeepEqual(waits.list(), []time.Duration{15 * time.Second, 30 * time.Second}) {
		t.Fatalf("receipt %+v posts %d waits %v", r, x.exitPosts(), waits.list())
	}
	a, err := x.c.GetAgent(context.Background(), x.e.task, x.e.agent)
	if newest := x.newest(t); err != nil || a.Status != api.AgentExited || a.RunID != x.e.runID || newest.Kind != api.EventExited || newest.Text != "Process exited (1)" {
		t.Fatalf("agent %s %s %v newest %+v", a.Status, a.RunID, err, newest)
	}
}

// e2: the retry waits 15s, 30s, 1m, 2m and 4m, then every 5 minutes, never longer.
func TestWrapperExitReportBackoffIsBounded(t *testing.T) {
	x := newExitReportHub(t)
	x.answer = func(int) int { return http.StatusServiceUnavailable }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits recordedWaits
	before := x.receipt(t)
	x.reporter(t, cancel, 9, &waits).deliver(ctx, x.e, 1)
	m := time.Minute
	if want := []time.Duration{15 * time.Second, 30 * time.Second, m, 2 * m, 4 * m, 5 * m, 5 * m, 5 * m, 5 * m}; !reflect.DeepEqual(waits.list(), want) {
		t.Fatalf("waits %v, want %v", waits.list(), want)
	}
	if after := x.receipt(t); after != before || x.exitPosts() != 8 {
		t.Fatalf("receipt %+v posts %d", after, x.exitPosts())
	}
}

// e2: the retry ends with the session: the context spawn.Wrap's return
// cancels. Nothing is posted or recorded.
func TestWrapperExitReportStopsWhenSessionEnds(t *testing.T) {
	x := newExitReportHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := productionExitReporter(x.e)
	waiting := make(chan struct{})
	r.after = func(time.Duration) <-chan time.Time {
		close(waiting)
		return make(chan time.Time)
	}
	r.table = func(context.Context) (map[int]discoveredProcess, error) { return idleShellTable(x.receipt(t)), nil }
	r.self = func() int { return 4200 }
	before := x.receipt(t)
	done := make(chan struct{})
	go func() {
		r.deliver(ctx, x.e, 1)
		close(done)
	}()
	<-waiting
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the retry outlived its session")
	}
	if after := x.receipt(t); after != before || x.exitPosts() != 0 {
		t.Fatalf("receipt %+v posts %d", after, x.exitPosts())
	}
}

// e2: a refusal that shows the hub already holds this run's exit counts as
// accepted; any other lasting refusal stops the retry and is recorded; a
// rate limit, a server error or a lost connection is retried.
func TestWrapperExitReportRefusals(t *testing.T) {
	cases := []struct {
		name string
		// before changes the hub before the retry; answer scripts the first post.
		before   func(t *testing.T, x *exitReportHub)
		answer   int
		posts    int
		accepted bool
		stopped  string
	}{
		{"already exited at this run", func(t *testing.T, x *exitReportHub) {
			// An earlier post reached the hub; its response was lost.
			if _, err := x.c.PostEvent(context.Background(), x.e.task, api.PostEventRequest{AgentID: x.e.agent, RunID: x.e.runID, Kind: api.EventExited, Text: "Process exited (1)"}); err != nil {
				t.Fatal(err)
			}
		}, 0, 2, true, ""},
		{"closed", func(t *testing.T, x *exitReportHub) {
			if _, err := x.c.CloseAgent(context.Background(), x.e.task, x.e.agent, x.e.runID); err != nil {
				t.Fatal(err)
			}
		}, 0, 1, false, "the hub refused the exit report: 409 "},
		{"run mismatch", nil, http.StatusBadRequest, 1, false, "the hub refused the exit report: 400 fixture refusal"},
		{"unauthorized", nil, http.StatusUnauthorized, 1, false, "the hub refused the exit report: 401 fixture refusal"},
		{"rate limited", nil, http.StatusTooManyRequests, 2, true, ""},
		{"server error", nil, http.StatusInternalServerError, 2, true, ""},
		{"lost connection", nil, -1, 2, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := newExitReportHub(t)
			if c.before != nil {
				c.before(t, x)
			}
			x.answer = func(n int) int {
				if n == 0 {
					return c.answer
				}
				return 0
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var waits recordedWaits
			// The retry must end by itself; ten waits mean it did not.
			x.reporter(t, cancel, 10, &waits).deliver(ctx, x.e, 1)
			r := x.receipt(t)
			if x.exitPosts() != c.posts || r.ExitReportedAt.IsZero() != !c.accepted || !strings.HasPrefix(r.ExitReportStopped, c.stopped) || (c.stopped == "") != (r.ExitReportStopped == "") || len(waits.list()) >= 10 {
				t.Fatalf("posts %d receipt %+v waits %v", x.exitPosts(), r, waits.list())
			}
			if c.name == "closed" && !strings.Contains(r.ExitReportStopped, "the agent is closed on "+x.e.runID) {
				t.Fatalf("stopped: %s", r.ExitReportStopped)
			}
		})
	}
}

// e2: an exit is never reported from a receipt without the recorded exit or
// of another run, and no retry starts when the exit could not be recorded.
func TestWrapperExitReportNeedsRecordedExit(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, x *exitReportHub)
	}{
		{"no recorded exit", func(t *testing.T, x *exitReportHub) { x.writeReceipt(t, func(*runtimeProcessReceipt) {}) }},
		{"another run", func(t *testing.T, x *exitReportHub) {
			x.writeReceipt(t, func(r *runtimeProcessReceipt) { r.Run, r.ExitedAt = "run_00000000000000cc", time.Now().UTC() })
		}},
		{"another session", func(t *testing.T, x *exitReportHub) {
			x.writeReceipt(t, func(r *runtimeProcessReceipt) { r.Session, r.ExitedAt = "tt-handler-other", time.Now().UTC() })
		}},
		{"missing receipt", func(t *testing.T, x *exitReportHub) {
			if err := os.Remove(runtimeProcessPath(x.e.hub, x.e.agent, x.e.runID)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newExitReportHub(t)
			c.change(t, x)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var waits recordedWaits
			x.reporter(t, cancel, 5, &waits).deliver(ctx, x.e, 1)
			if x.exitPosts() != 0 || len(waits.list()) != 1 {
				t.Fatalf("posts %d waits %v", x.exitPosts(), waits.list())
			}
		})
	}
	// The wrapper's exit could not be recorded: its post fails and no retry starts.
	x := newExitReportHub(t)
	if err := os.Remove(runtimeProcessPath(x.e.hub, x.e.agent, x.e.runID)); err != nil {
		t.Fatal(err)
	}
	x.answer = func(int) int { return http.StatusServiceUnavailable }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits recordedWaits
	if wrapperExited(ctx, x.e, 1, x.reporter(t, cancel, 0, &waits)) || x.exitPosts() != 1 || len(waits.list()) != 0 {
		t.Fatalf("a retry started without a recorded exit: posts %d waits %v", x.exitPosts(), waits.list())
	}
	if _, err := os.Stat(runtimeProcessPath(x.e.hub, x.e.agent, x.e.runID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a receipt was synthesized: %v", err)
	}
}

// e3: the retry posts only while the pane holds just the wrapper and its idle
// login shell. Anything under the shell, a runtime exec'd in it or an
// unreadable table waits; a receipt without this wrapper's pane stops it.
func TestWrapperExitReportWaitsForIdleShell(t *testing.T) {
	t.Run("busy then idle", func(t *testing.T) {
		x := newExitReportHub(t)
		reads := 0
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var waits recordedWaits
		r := x.reporter(t, cancel, 10, &waits)
		r.table = func(context.Context) (map[int]discoveredProcess, error) {
			reads++
			table := idleShellTable(x.receipt(t))
			if reads == 1 {
				table[4400] = discoveredProcess{4400, 4300, table[4300].started, "claude"}
			}
			return table, nil
		}
		r.deliver(ctx, x.e, 1)
		if reads != 2 || x.exitPosts() != 1 || x.receipt(t).ExitReportedAt.IsZero() {
			t.Fatalf("reads %d posts %d receipt %+v", reads, x.exitPosts(), x.receipt(t))
		}
	})
	for _, c := range []struct {
		name  string
		table func(map[int]discoveredProcess) (map[int]discoveredProcess, error)
	}{
		{"runtime exec'd in the shell", func(m map[int]discoveredProcess) (map[int]discoveredProcess, error) {
			m[4300] = discoveredProcess{4300, 4200, m[4300].started, "codex"}
			return m, nil
		}},
		{"process under the shell", func(m map[int]discoveredProcess) (map[int]discoveredProcess, error) {
			m[4400] = discoveredProcess{4400, 4300, m[4300].started, "sleep"}
			return m, nil
		}},
		{"shell started before the exit", func(m map[int]discoveredProcess) (map[int]discoveredProcess, error) {
			m[4300] = discoveredProcess{4300, 4200, "Wed Oct  7 06:00:01 2026", "-zsh"}
			return m, nil
		}},
		{"unreadable table", func(map[int]discoveredProcess) (map[int]discoveredProcess, error) {
			return nil, errors.New("unreadable process table row")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newExitReportHub(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var waits recordedWaits
			r := x.reporter(t, cancel, 3, &waits)
			r.table = func(context.Context) (map[int]discoveredProcess, error) { return c.table(idleShellTable(x.receipt(t))) }
			before := x.receipt(t)
			r.deliver(ctx, x.e, 1)
			if x.exitPosts() != 0 || x.receipt(t) != before || len(waits.list()) != 3 {
				t.Fatalf("posts %d receipt %+v waits %v", x.exitPosts(), x.receipt(t), waits.list())
			}
		})
	}
	for _, c := range []struct {
		name   string
		change func(*runtimeProcessReceipt)
	}{
		{"pane process of another wrapper", func(r *runtimeProcessReceipt) { r.PanePID = 4201 }},
		{"no pane process", func(r *runtimeProcessReceipt) { r.PanePID, r.PaneStarted = 0, "" }},
		{"no login shell", func(r *runtimeProcessReceipt) { r.Shell = "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newExitReportHub(t)
			x.writeReceipt(t, func(r *runtimeProcessReceipt) { c.change(r); r.ExitedAt = time.Now().UTC() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var waits recordedWaits
			x.reporter(t, cancel, 3, &waits).deliver(ctx, x.e, 1)
			if r := x.receipt(t); x.exitPosts() != 0 || r.ExitReportStopped == "" || !r.ExitReportedAt.IsZero() || len(waits.list()) != 1 {
				t.Fatalf("posts %d receipt %+v waits %v", x.exitPosts(), r, waits.list())
			}
		})
	}
	t.Run("the wrapper's own ps is not a child of the pane", func(t *testing.T) {
		receipt := runtimeProcessReceipt{PanePID: 4200, PaneStarted: "Wed Oct  7 06:00:00 2026", Shell: "/bin/zsh", ExitedAt: time.Date(2026, 10, 7, 13, 0, 0, 0, time.Local)}
		raw := "    1     0 Sat Aug 22 19:33:24 2026     /sbin/launchd\n 4200     1 Wed Oct  7 06:00:00 2026     tt\n" +
			" 4300  4200 Wed Oct  7 13:00:01 2026     -zsh\n 4500  4200 Wed Oct  7 13:00:05 2026     ps\n"
		all, err := parseProcessTable(raw)
		if err != nil {
			t.Fatal(err)
		}
		if state, _ := idleShellTree(all, receipt); state != handlerProbeUnknown {
			t.Fatalf("with its own ps the pane answers %s", state)
		}
		own, err := parseWrapperProcessTable(raw, 4500)
		if _, present := own[4500]; err != nil || present || len(own) != 3 {
			t.Fatalf("table %+v %v", own, err)
		}
		if state, detail := idleShellTree(own, receipt); state != handlerProbeGone {
			t.Fatalf("without its own ps the pane answers %s (%s)", state, detail)
		}
		if _, err := parseWrapperProcessTable("not a table", 4500); err == nil {
			t.Fatal("an unreadable table was read")
		}
	})
}
