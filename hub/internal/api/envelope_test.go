package api

import (
	"reflect"
	"strings"
	"testing"
)

// validEnvelopes has one minimal valid envelope per kind.
func validEnvelopes() map[string]Envelope {
	ev := map[string]Evidence{"e1": {Type: "command", Value: "go test ./cmd/tt", Outcome: "ok"}}
	return map[string]Envelope{
		EnvelopeKindAssign: {Kind: "assign", To: "builder", Subject: "Reject empty recipient in tt post",
			Body: EnvelopeBody{Objective: "Fail instead of posting to the board", Owns: []string{"hub/cmd/tt/main.go"},
				Acceptance: map[string]string{"a1": "tt post --to '' exits 2", "a2": "go test ./cmd/tt passes"}}, Due: "45m"},
		EnvelopeKindRequest:  {Kind: "request", Subject: "Record the order for the builder", Body: EnvelopeBody{Ask: "Save the governing order"}},
		EnvelopeKindReview:   {Kind: "review", Subject: "Review the empty recipient fix", Refs: map[string]string{"commit": "abc1234"}, Body: EnvelopeBody{Candidate: "abc1234", Scope: "tt post", Acceptance: map[string]string{"a1": "exits 2"}}},
		EnvelopeKindQuestion: {Kind: "question", Subject: "Which status code for rejected posts", Body: EnvelopeBody{Question: "Should rejected posts return 422?"}},
		EnvelopeKindResult:   {Kind: "result", Subject: "Empty recipient fix passes its checks", Body: EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass", "a2": "pass"}}, Evidence: ev},
		EnvelopeKindAnswer:   {Kind: "answer", Subject: "Use 422 for rejected posts", Body: EnvelopeBody{Answer: "422 with reasons"}},
		EnvelopeKindBlock:    {Kind: "block", Subject: "Waiting on the review blocker list", Body: EnvelopeBody{Reason: "no blocker list yet", Needs: "reviewer", ResumeWhen: "round one arrives"}},
		EnvelopeKindDecline:  {Kind: "decline", Subject: "Declining the schema change request", Body: EnvelopeBody{Reason: "outside owned files"}},
		EnvelopeKindFinding:  {Kind: "finding", Subject: "Whitespace-only posts are accepted", Body: EnvelopeBody{Severity: "high", Summary: "store checks empty text before trimming"}, Evidence: ev},
		EnvelopeKindNotice:   {Kind: "notice", Subject: "Integration window is closed today", Body: EnvelopeBody{Text: "Rebase on the release root"}},
	}
}

func problemFields(ps []Problem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Field)
	}
	return out
}

func hasProblem(ps []Problem, field string) bool {
	for _, p := range ps {
		if p.Field == field {
			return true
		}
	}
	return false
}

func TestValidEnvelopePerKind(t *testing.T) {
	for kind, e := range validEnvelopes() {
		if ps := ValidateEnvelope(e); ps != nil {
			t.Errorf("%s: unexpected problems %v", kind, ps)
		}
	}
}

func TestEnvelopeRequiredFieldsPerKind(t *testing.T) {
	want := map[string][]string{
		EnvelopeKindAssign:   {"body.objective", "body.owns", "body.acceptance"},
		EnvelopeKindRequest:  {"body.ask"},
		EnvelopeKindReview:   {"body.candidate", "body.scope", "body.acceptance"},
		EnvelopeKindQuestion: {"body.question"},
		EnvelopeKindResult:   {"body.outcome", "body.status", "evidence"},
		EnvelopeKindAnswer:   {"body.answer"},
		EnvelopeKindBlock:    {"body.reason", "body.needs", "body.resumeWhen"},
		EnvelopeKindDecline:  {"body.reason"},
		EnvelopeKindFinding:  {"body.severity", "body.summary", "evidence"},
		EnvelopeKindNotice:   {"body.text"},
	}
	for kind, fields := range want {
		ps := ValidateEnvelope(Envelope{Kind: kind, Subject: "A plain English subject line"})
		if got := problemFields(ps); !reflect.DeepEqual(got, fields) {
			t.Errorf("%s: problems %v, want %v", kind, got, fields)
		}
	}
	if ps := ValidateEnvelope(Envelope{Kind: "ASSIGN", Subject: "A plain English subject line"}); !hasProblem(ps, "kind") {
		t.Errorf("unknown kind accepted: %v", ps)
	}
}

func TestEnvelopeSubjectRules(t *testing.T) {
	base := validEnvelopes()[EnvelopeKindNotice]
	for _, tc := range []struct{ subject, reason string }{
		{"Too short", "at least 10"},
		{strings.Repeat("x", 121), "at most 120"},
		{"Saved the record\nfor the item", "one line"},
		{"Saved the order for wi_74c050c73f7ff0b4", "record IDs"},
		{"Released candidate 43c1da3 to production", "hashes"},
		{"Wrote the report to /tmp/report.json today", "paths"},
		{"Wrote the report to ~/notes/report.md today", "paths"},
	} {
		e := base
		e.Subject = tc.subject
		ps := ValidateEnvelope(e)
		if !hasProblem(ps, "subject") || !strings.Contains(ps[0].Reason, tc.reason) {
			t.Errorf("subject %q: problems %v, want reason containing %q", tc.subject, ps, tc.reason)
		}
	}
	for _, ok := range []string{"Fixed the release for 2026 schedule", "Deadbeef cache fixed in the board view", "Board and Teams share one layout"} {
		e := base
		e.Subject = ok
		if ps := ValidateEnvelope(e); ps != nil {
			t.Errorf("subject %q rejected: %v", ok, ps)
		}
	}
}

func TestEnvelopeLimitsAndKeys(t *testing.T) {
	e := validEnvelopes()[EnvelopeKindAssign]
	e.Body.Acceptance = map[string]string{"A1": "upper", "a2": " "}
	e.Refs = map[string]string{"bad key": "x"}
	e.Evidence = map[string]Evidence{"e1": {Type: "vibes", Value: ""}}
	e.Due = "-5m"
	ps := ValidateEnvelope(e)
	for _, f := range []string{"body.acceptance.A1", "body.acceptance.a2", "refs.bad key", "evidence.e1.type", "evidence.e1.value", "due"} {
		if !hasProblem(ps, f) {
			t.Errorf("missing problem for %s in %v", f, ps)
		}
	}
	r := validEnvelopes()[EnvelopeKindResult]
	r.Body.Status = map[string]string{"a1": "maybe"}
	if !hasProblem(ValidateEnvelope(r), "body.status.a1") {
		t.Error("invalid status value accepted")
	}
	big := validEnvelopes()[EnvelopeKindNotice]
	big.Body.Text = strings.Repeat("x", MaxEnvelopeBodyBytes)
	if !hasProblem(ValidateEnvelope(big), "body") {
		t.Error("oversized body accepted")
	}
	huge := validEnvelopes()[EnvelopeKindNotice]
	huge.Attachments = []string{strings.Repeat("y", MaxEnvelopeBytes)}
	if !hasProblem(ValidateEnvelope(huge), "envelope") {
		t.Error("oversized envelope accepted")
	}
	q := validEnvelopes()[EnvelopeKindQuestion]
	q.Body.Question = "Should it be 422? Or 400?"
	if !hasProblem(ValidateEnvelope(q), "body.question") {
		t.Error("two questions accepted")
	}
	if e := (Envelope{Kind: "notice", Subject: "A plain English subject line", Body: EnvelopeBody{Text: "x"}, Due: "200h"}); !hasProblem(ValidateEnvelope(e), "due") {
		t.Error("due beyond a week accepted")
	}
}

func TestRenderTextRoundTripsThroughConvention(t *testing.T) {
	for kind, e := range validEnvelopes() {
		text := RenderText(e)
		if !strings.HasPrefix(text, strings.ToUpper(kind)+": ") {
			t.Fatalf("%s: rendered %q", kind, text)
		}
		parsed, matched := ParseTextConvention(text)
		if !matched {
			t.Fatalf("%s: rendered text not recognized: %q", kind, text)
		}
		if ps := ValidateEnvelope(parsed); ps != nil {
			t.Errorf("%s: parsed rendering invalid %v\n%s", kind, ps, text)
		}
		if again := RenderText(parsed); again != text {
			t.Errorf("%s: render is not stable\nfirst:  %q\nsecond: %q", kind, text, again)
		}
	}
}

func TestRenderTextOrdersNamedKeysNaturally(t *testing.T) {
	e := Envelope{Kind: "assign", Subject: "Ordering check for criteria", Body: EnvelopeBody{Objective: "x", Owns: []string{"f"},
		Acceptance: map[string]string{"a10": "ten", "a2": "two", "a1": "one"}}}
	if !strings.Contains(RenderText(e), "Acceptance: a1: one; a2: two; a10: ten") {
		t.Fatalf("rendered %q", RenderText(e))
	}
}

func TestParseTextConventionClassification(t *testing.T) {
	if _, matched := ParseTextConvention("SAVED activation release under exact lead8726"); matched {
		t.Error("unknown uppercase word treated as a kind")
	}
	if _, matched := ParseTextConvention("hey can someone look at the relay"); matched {
		t.Error("free text matched")
	}
	e, matched := ParseTextConvention("RESULT: Tests pass for the recipient check\nRefs: commit=abc1234, item=wi_1\nOutcome: done\nStatus: a1 pass, a2 fail\nEvidence: e1: go test ./cmd/tt -> ok; e2: abc1234")
	if !matched {
		t.Fatal("convention not matched")
	}
	want := map[string]Evidence{"e1": {Type: "command", Value: "go test ./cmd/tt", Outcome: "ok"}, "e2": {Type: "commit", Value: "abc1234"}}
	if !reflect.DeepEqual(e.Evidence, want) || e.Refs["commit"] != "abc1234" || e.Body.Status["a2"] != "fail" {
		t.Fatalf("parsed %+v", e)
	}
	if ps := ValidateEnvelope(e); ps != nil {
		t.Fatalf("problems %v", ps)
	}
	acceptance, _ := ParseTextConvention("ASSIGN: Reject the empty recipient\nObjective: fail fast\nOwns: a.go\nAcceptance: a1: exits 2, prints recipient required")
	if !reflect.DeepEqual(acceptance.Body.Acceptance, map[string]string{"a1": "exits 2, prints recipient required"}) {
		t.Fatalf("comma inside one criterion split: %v", acceptance.Body.Acceptance)
	}
}
