package jevkit

import (
	"testing"
	"time"
)

var now = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
var pol = Policy{MaxWakes: 3, Window: 10 * time.Minute, ResumeRetiredOnDirect: true}

func ag() Agent { return Agent{ID: "agt_1", Name: "builder", Status: "active"} }

type tc struct {
	name string
	m    Message
	a    func(Agent) Agent
	p    func(Policy) Policy
	want Decision
}

var cases = []tc{
	{"R1 self", Message{Seq: 5, FromAgentID: "agt_1", To: "builder"}, nil, nil, Decision{Action: "skip", Reason: "self"}},
	{"R2 closed", Message{Seq: 5, To: "builder"}, func(a Agent) Agent { a.Status = "closed"; return a }, nil, Decision{Action: "skip", Reason: "closed"}},
	{"R2 closed beats busy", Message{Seq: 5, To: "builder"}, func(a Agent) Agent { a.Status = "closed"; a.Busy = true; return a }, nil, Decision{Action: "skip", Reason: "closed"}},
	{"R3 dup equal", Message{Seq: 5, To: "builder"}, func(a Agent) Agent { a.LastWokenSeq = 5; return a }, nil, Decision{Action: "skip", Reason: "duplicate"}},
	{"R4 other recipient", Message{Seq: 5, FromAgentID: "agt_2", To: "reviewer"}, nil, nil, Decision{Action: "skip", Reason: "not_recipient"}},
	{"R4 boardwide non-orch", Message{Seq: 5, FromAgentID: "agt_2"}, nil, nil, Decision{Action: "skip", Reason: "not_recipient"}},
	{"R4 boardwide orch", Message{Seq: 5, FromAgentID: "agt_2"}, func(a Agent) Agent { a.Orchestrator = true; return a }, nil, Decision{Action: "wake", Reason: "eligible"}},
	{"R5 agent ack", Message{Seq: 5, FromAgentID: "agt_2", To: "builder", IsAck: true}, nil, nil, Decision{Action: "skip", Reason: "ack"}},
	{"R5 human ack wakes", Message{Seq: 5, To: "builder", IsAck: true}, nil, nil, Decision{Action: "wake", Reason: "eligible"}},
	{"R6 paused agent msg", Message{Seq: 5, FromAgentID: "agt_2", To: "builder"}, nil, func(p Policy) Policy { p.ProjectPaused = true; return p }, Decision{Action: "defer", Reason: "paused"}},
	{"R6 paused human msg", Message{Seq: 5, To: "builder"}, nil, func(p Policy) Policy { p.ProjectPaused = true; return p }, Decision{Action: "wake", Reason: "eligible"}},
	{"R7 retired direct", Message{Seq: 5, FromAgentID: "agt_2", To: "builder"}, func(a Agent) Agent { a.Status = "retired"; return a }, nil, Decision{Action: "resume", Reason: "direct_to_retired"}},
	{"R7 retired swarm", Message{Seq: 5, FromAgentID: "agt_2", Swarm: true}, func(a Agent) Agent { a.Status = "retired"; return a }, nil, Decision{Action: "skip", Reason: "retired"}},
	{"R8 busy", Message{Seq: 5, To: "builder"}, func(a Agent) Agent { a.Busy = true; return a }, nil, Decision{Action: "defer", Reason: "busy"}},
	{"R9 rate limited retryAt=oldest+window", Message{Seq: 5, FromAgentID: "agt_2", To: "builder"},
		func(a Agent) Agent {
			a.RecentWakes = []time.Time{now.Add(-1 * time.Minute), now.Add(-8 * time.Minute), now.Add(-4 * time.Minute), now.Add(-30 * time.Minute)}
			return a
		}, nil, Decision{Action: "defer", Reason: "rate_limited", RetryAt: now.Add(2 * time.Minute)}},
	{"R9 human bypass", Message{Seq: 5, To: "builder"},
		func(a Agent) Agent { a.RecentWakes = []time.Time{now, now, now}; return a }, nil, Decision{Action: "wake", Reason: "eligible"}},
}

var impls = map[string]func(Message, Agent, Policy, time.Time) Decision{
	"ref": ShouldWake, "A": ShouldWakeA, "B": ShouldWakeB, "C": ShouldWakeC, "D": ShouldWakeD,
	"E": ShouldWakeE, "F": ShouldWakeF, "G": ShouldWakeG, "H": ShouldWakeH,
}

func TestGroundTruth(t *testing.T) {
	for _, k := range []string{"ref", "A", "B", "C", "D", "E", "F", "G", "H"} {
		var failed []string
		for _, c := range cases {
			a, p := ag(), pol
			if c.a != nil { a = c.a(a) }
			if c.p != nil { p = c.p(p) }
			if got := impls[k](c.m, a, p, now); got != c.want {
				failed = append(failed, c.name)
			}
		}
		t.Logf("%-3s failing: %v", k, failed)
	}
}
