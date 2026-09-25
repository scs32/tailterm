package server

import (
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

func TestWithdrawHTTPRequiresOriginalSendingRun(t *testing.T) {
	f := newOblFixture(t)
	m := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: f.builder.ID, Envelope: assignFrom(f.lead, f.builder.Name)})
	list := f.list(t, store.ObligationFilter{}, m.CreatedAt)
	if len(list) != 1 {
		t.Fatalf("obligations: %+v", list)
	}
	path := "/v1/tasks/" + f.task.ID + "/obligations/" + list[0].ID + "/withdraw"
	for _, req := range []api.ObligationWithdrawRequest{
		{AgentID: f.builder.ID, RunID: f.builder.RunID, Reason: "new order"},
		{AgentID: f.lead.ID, RunID: "run_0000000000000000", Reason: "new order"},
		{AgentID: f.lead.ID, RunID: f.lead.RunID, Reason: "  "},
	} {
		var out api.Obligation
		if code := f.c.do("POST", path, req, &out); code < 400 {
			t.Fatalf("accepted bad request %+v: %d %+v", req, code, out)
		}
	}
	good := api.ObligationWithdrawRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Reason: "new order", RequestID: "http-withdraw"}
	var out api.Obligation
	if code := f.c.do("POST", path, good, &out); code != 200 || out.Outcome != api.OutcomeWithdrawn {
		t.Fatalf("withdraw: %d %+v", code, out)
	}
	if code := f.c.do("POST", path, good, &out); code != 200 || out.Outcome != api.OutcomeWithdrawn {
		t.Fatalf("retry: %d %+v", code, out)
	}
	good.Reason = "changed"
	if code := f.c.do("POST", path, good, &out); code != 409 {
		t.Fatalf("changed retry: %d", code)
	}
	var obligations api.ObligationList
	if code := f.c.do("GET", "/v1/tasks/"+f.task.ID+"/obligations?open=1", nil, &obligations); code != 200 || len(obligations.Obligations) != 1 || obligations.Obligations[0].SourceKind != api.EnvelopeKindNotice {
		t.Fatalf("open obligations after withdrawal: %d %+v", code, obligations)
	}
}
