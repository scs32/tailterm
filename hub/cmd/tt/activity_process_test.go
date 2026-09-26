package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	t.Setenv(spawn.EnvSession, "fake-session")
	t.Setenv("TT_TMUX_SOCKET", "synthetic-activity-process")
	ps := filepath.Join(home, "ps")
	if err := os.WriteFile(ps, []byte("#!/bin/sh\nprintf 'synthetic-start\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	tmux := filepath.Join(home, "tmux")
	line := `["$1","123","fake-session","https://synthetic.invalid","tsk_0123456789abcdef","agt_0123456789abcdef","run_0123456789abcdef"]`
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
	if err = json.Unmarshal(data, &receipt); err != nil || receipt.PID != 4321 || receipt.Started != "synthetic-start" || receipt.Session != "fake-session" || receipt.Socket != "synthetic-activity-process" || receipt.SessionID != "$1" || receipt.SessionCreated != "123" {
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
