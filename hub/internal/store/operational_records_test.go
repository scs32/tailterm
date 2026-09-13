package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"strings"
	"sync"
	"testing"
)

type opFixture struct {
	deliveryFixture
	d           api.RequiredDelivery
	instruction api.OperationalRecord
	n           int
}

func opFixtureNew(t *testing.T) *opFixture {
	f := newDeliveryFixture(t)
	d := f.directive(t, "Exact operational directive", "op-directive", api.DeliveryAssignment, 0, "")
	return &opFixture{deliveryFixture: f, d: d.Delivery}
}
func (f *opFixture) key() string { f.n++; return fmt.Sprintf("op-%d", f.n) }
func (f *opFixture) data(k string) api.OperationalData {
	return api.OperationalData{Kind: k, ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, ScopeRevision: f.item.ScopeRevision, DeliveryID: f.d.ID, Generation: f.d.Generation, Epoch: f.d.ExecutionEpoch, Sources: []api.OperationalSource{{Message: api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq}, TextDigest: operationalDigest([]byte(f.order.Text))}}}
}
func opRef(r api.OperationalRecord) api.OperationalReference {
	return api.OperationalReference{ID: r.ID, Version: r.Version}
}
func (f *opFixture) propose(t *testing.T, d api.OperationalData, actor api.Agent) api.OperationalMutation {
	t.Helper()
	out, err := f.s.ProposeOperationalRecord(context.Background(), f.task.ID, api.ProposeOperationalRecordRequest{RequestID: f.key(), AgentID: actor.ID, RunID: actor.RunID, Data: d}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func (f *opFixture) commit(t *testing.T, d api.OperationalData, actor api.Agent) api.OperationalRecord {
	t.Helper()
	p := f.propose(t, d, actor)
	out, err := f.s.CommitOperationalRecord(context.Background(), f.task.ID, p.Record.ID, api.CommitOperationalRecordRequest{RequestID: f.key(), ExpectedVersion: 1, AgentID: actor.ID, RunID: actor.RunID}, f.by)
	if err != nil {
		t.Fatalf("%s: %v", d.Kind, err)
	}
	if out.Event == nil || out.Record.State != "committed" {
		t.Fatal("missing atomic event")
	}
	return out.Record
}
func (f *opFixture) start(t *testing.T) {
	d := f.data("instruction")
	d.Instruction = &api.OperationalInstruction{Objective: "Fence obsolete work", Scope: []string{"fixture"}, Criteria: []api.OperationalCriterion{{ID: "stale", Description: "obsolete epoch conflicts"}}}
	f.instruction = f.commit(t, d, f.lead)
	_, err := f.s.AcknowledgeDelivery(context.Background(), f.task.ID, f.d.ID, api.DeliveryActionRequest{RequestID: f.key(), AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}, f.by)
	if err != nil {
		t.Fatal(err)
	}
}
func (f *opFixture) artifact(t *testing.T, body any) api.OperationalArtifact {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	key := f.key()
	a, _, err := f.s.PutNarrativeArtifact(context.Background(), f.task.ID, f.item.ID, api.PutNarrativeArtifactRequest{RequestID: key, Namespace: "operational-test", SourceID: key, SourceVersion: "1", Kind: "evidence", Title: "Synthetic evidence", OriginalAuthor: api.Sender{AgentID: f.worker.ID}, Provenance: "native", CaptureState: "stored-content", Availability: "available", Content: string(raw)}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return api.OperationalArtifact{ID: a.ArtifactID, Version: a.Version, Digest: a.ContentDigest}
}
func (f *opFixture) candidate(t *testing.T) api.OperationalRecord {
	d := f.data("finding")
	d.Finding = &api.OperationalFinding{Instruction: opRef(f.instruction), Observation: "Old epoch accepted", Reproduction: []string{"resume then use old epoch"}}
	finding := f.commit(t, d, f.worker)
	commit := strings.Repeat("a", 40)
	manifest := f.artifact(t, api.OperationalCandidateEvidence{Version: 1, Commit: commit, Files: []api.OperationalCandidateFile{{Path: "fixture.go", Digest: strings.Repeat("b", 64)}}})
	d = f.data("candidate")
	d.Candidate = &api.OperationalCandidate{Instruction: opRef(f.instruction), Findings: []api.OperationalReference{opRef(finding)}, Commit: commit, Artifact: manifest}
	return f.commit(t, d, f.worker)
}
func (f *opFixture) verificationData(t *testing.T, c api.OperationalRecord, outcome string) api.OperationalData {
	ev := api.OperationalTestEvidence{Version: 1, Candidate: opRef(c), Commit: c.Data.Candidate.Commit, CandidateDigest: c.Data.Candidate.Artifact.Digest, CriterionID: "stale", TestID: "TestStale", Command: []string{"go", "test", "-run", "TestStale"}, Outcome: outcome, AgentID: f.worker.ID, RunID: f.worker.RunID}
	d := f.data("verification")
	d.Verification = &api.OperationalVerification{Candidate: ev.Candidate, CriterionID: ev.CriterionID, TestID: ev.TestID, Command: ev.Command, Outcome: outcome, Evidence: f.artifact(t, ev)}
	return d
}
func (f *opFixture) reject(t *testing.T, d api.OperationalData, actor api.Agent) {
	t.Helper()
	p := f.propose(t, d, f.worker)
	ctx := context.Background()
	before, _ := f.s.CurrentAssignment(ctx, f.task.ID, f.worker.ID, f.worker.RunID)
	var receipts int
	f.s.db.QueryRow(`SELECT COUNT(*) FROM delivery_operation_receipts`).Scan(&receipts)
	_, err := f.s.CommitOperationalRecord(ctx, f.task.ID, p.Record.ID, api.CommitOperationalRecordRequest{RequestID: f.key(), ExpectedVersion: 1, AgentID: actor.ID, RunID: actor.RunID}, f.by)
	if err == nil {
		t.Fatal("invalid commit accepted")
	}
	after, _ := f.s.CurrentAssignment(ctx, f.task.ID, f.worker.ID, f.worker.RunID)
	var count int
	f.s.db.QueryRow(`SELECT COUNT(*) FROM delivery_operation_receipts`).Scan(&count)
	saved, _ := f.s.GetOperationalRecord(ctx, f.task.ID, p.Record.ID, 0)
	if len(before.Events) != len(after.Events) || count != receipts || saved.State != "proposed" || saved.Version != 1 {
		t.Fatal("rejection mutated event/receipt/state")
	}
}
func TestOperationalLifecycle(t *testing.T) {
	f := opFixtureNew(t)
	f.start(t)
	c := f.candidate(t)
	v := f.commit(t, f.verificationData(t, c, "pass"), f.worker)
	d := f.data("result")
	d.Result = &api.OperationalResult{Candidate: opRef(c), Verifications: []api.OperationalReference{opRef(v)}, Summary: "Exact candidate tested"}
	r := f.commit(t, d, f.worker)
	current, err := f.s.CurrentAssignment(context.Background(), f.task.ID, f.worker.ID, f.worker.RunID)
	if err != nil || current.Phase != api.DeliveryResult {
		t.Fatal("typed result did not update existing execution projection")
	}
	d = f.data("acceptance")
	d.Acceptance = &api.OperationalAcceptance{Result: opRef(r), Decision: "accepted", Reason: "Exact criterion passed"}
	f.reject(t, d, f.worker)
	accepted := f.commit(t, d, f.lead)
	if accepted.Sources[0].Author != f.order.From || accepted.Sources[0].Text != f.order.Text {
		t.Fatal("source not derived from immutable message")
	}
	old, err := f.s.GetOperationalRecord(context.Background(), f.task.ID, accepted.ID, 1)
	if err != nil || old.State != "proposed" || len(old.Sources) != 0 {
		t.Fatal("proposal history was rewritten")
	}
	item, err := f.s.GetWorkItem(context.Background(), f.task.ID, f.item.ID)
	if err != nil || item.Status == "done" {
		t.Fatal("typed acceptance must not complete item")
	}
}
func TestOperationalIncompleteAndContradictory(t *testing.T) {
	t.Run("incomplete", func(t *testing.T) {
		f := opFixtureNew(t)
		p := f.propose(t, api.OperationalData{Kind: "instruction"}, f.lead)
		if p.Event != nil {
			t.Fatal("proposal creates event")
		}
		f.reject(t, api.OperationalData{Kind: "instruction"}, f.lead)
		current, _ := f.s.CurrentAssignment(context.Background(), f.task.ID, f.worker.ID, f.worker.RunID)
		if current.Phase != api.DeliveryUnacknowledged || len(current.Events) != 1 {
			t.Fatal("proposal acknowledged work")
		}
	})
	t.Run("failed evidence", func(t *testing.T) {
		f := opFixtureNew(t)
		f.start(t)
		c := f.candidate(t)
		d := f.verificationData(t, c, "fail")
		v := f.commit(t, d, f.worker)
		d.Verification.Outcome = "pass"
		f.reject(t, d, f.worker)
		d = f.data("result")
		d.Result = &api.OperationalResult{Candidate: opRef(c), Verifications: []api.OperationalReference{opRef(v)}, Summary: "Failed candidate"}
		r := f.commit(t, d, f.worker)
		d = f.data("acceptance")
		d.Acceptance = &api.OperationalAcceptance{Result: opRef(r), Decision: "accepted", Reason: "prose claims pass"}
		f.reject(t, d, f.lead)
	})
}
func TestOperationalFenceRejections(t *testing.T) {
	for _, k := range []string{"source", "item", "scope", "generation", "epoch", "actor", "test", "artifact", "commit"} {
		t.Run(k, func(t *testing.T) {
			f := opFixtureNew(t)
			f.start(t)
			c := f.candidate(t)
			d := f.verificationData(t, c, "pass")
			actor := f.worker
			switch k {
			case "source":
				d.Sources[0].TextDigest = strings.Repeat("0", 64)
			case "item":
				d.ItemRevision++
			case "scope":
				d.ScopeRevision++
			case "generation":
				d.Generation++
			case "epoch":
				d.Epoch++
			case "actor":
				actor.RunID = api.NewID("run")
			case "test":
				d.Verification.TestID = ""
			case "artifact":
				d.Verification.Evidence.ID = "nart_0000000000000000"
			case "commit":
				d.Verification.Evidence = f.artifact(t, api.OperationalTestEvidence{Version: 1, Candidate: opRef(c), Commit: strings.Repeat("c", 40)})
			}
			f.reject(t, d, actor)
		})
	}
}
func TestOperationalReplayRestartAndInterleavedSource(t *testing.T) {
	f := opFixtureNew(t)
	ctx := context.Background()
	d := f.data("instruction")
	d.Instruction = &api.OperationalInstruction{Objective: "Exact source", Scope: []string{"fixture"}, Criteria: []api.OperationalCriterion{{ID: "source", Description: "correct source"}}}
	other, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "unrelated interleaved", RequestID: "other"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	req := api.ProposeOperationalRecordRequest{RequestID: "lost", AgentID: f.lead.ID, RunID: f.lead.RunID, Data: d}
	p, err := f.s.ProposeOperationalRecord(ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.s.ProposeOperationalRecord(ctx, f.task.ID, req, f.by)
	if err != nil || !replay.Replay || p.Record.ID != replay.Record.ID || p.Receipt.ID != replay.Receipt.ID {
		t.Fatal("proposal response loss")
	}
	req.Data.Kind = "finding"
	req.Data.Instruction = nil
	if _, err = f.s.ProposeOperationalRecord(ctx, f.task.ID, req, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed payload: %v", err)
	}
	commit := api.CommitOperationalRecordRequest{RequestID: "lost-commit", ExpectedVersion: 1, AgentID: f.lead.ID, RunID: f.lead.RunID}
	saved, err := f.s.CommitOperationalRecord(ctx, f.task.ID, p.Record.ID, commit, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Record.Sources[0].Message.Seq == other.Seq {
		t.Fatal("guessed source")
	}
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.Close()
	if _, err = f.s.CloseTask(ctx, f.task.ID, f.by); err != nil {
		t.Fatal(err)
	}
	replay, err = f.s.CommitOperationalRecord(ctx, f.task.ID, p.Record.ID, commit, f.by)
	if err != nil || !replay.Replay || replay.Receipt.ID != saved.Receipt.ID || replay.Event.ID != saved.Event.ID || replay.Record.Version != saved.Record.Version {
		t.Fatalf("historical replay: %+v %v", replay, err)
	}
}
func TestOperationalConcurrentProposalCAS(t *testing.T) {
	f := opFixtureNew(t)
	p := f.propose(t, api.OperationalData{Kind: "instruction"}, f.lead)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.s.ProposeOperationalRecord(context.Background(), f.task.ID, api.ProposeOperationalRecordRequest{RequestID: fmt.Sprintf("race-%d", i), RecordID: p.Record.ID, ExpectedVersion: 1, AgentID: f.lead.ID, RunID: f.lead.RunID, Data: api.OperationalData{Kind: "instruction", ItemRevision: int64(i + 1)}}, f.by)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	ok, conflict := 0, 0
	for err := range results {
		if err == nil {
			ok++
		} else if errors.Is(err, api.ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("CAS: %d %d", ok, conflict)
	}
}
