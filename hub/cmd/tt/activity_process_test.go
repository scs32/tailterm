package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
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
	recordRuntimeExit(e)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &receipt); err != nil || receipt.ExitedAt.IsZero() {
		t.Fatalf("exit receipt: %+v %v", receipt, err)
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
