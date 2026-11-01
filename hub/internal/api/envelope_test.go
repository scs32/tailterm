package api

import (
	"encoding/json"
	"fmt"
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

func TestEnvelopeVerificationCriteria(t *testing.T) {
	e := validEnvelopes()[EnvelopeKindAssign]
	e.Body.VerificationCriteria = []string{"a2"}
	if ps := ValidateEnvelope(e); ps != nil {
		t.Fatal(ps)
	}
	text := RenderText(e)
	parsed, ok := ParseTextConvention(text)
	if !ok || !reflect.DeepEqual(parsed.Body.VerificationCriteria, []string{"a2"}) {
		t.Fatal(text, parsed, ok)
	}
	e.Body.VerificationCriteria = []string{"a2", "a2", "a9"}
	if !hasProblem(ValidateEnvelope(e), "body.verificationCriteria.1") || !hasProblem(ValidateEnvelope(e), "body.verificationCriteria.2") {
		t.Fatal(ValidateEnvelope(e))
	}
	result := validEnvelopes()[EnvelopeKindResult]
	result.Body.Status["a2"] = "pending-verification"
	if ps := ValidateEnvelope(result); ps != nil {
		t.Fatal(ps)
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

func TestBrokerSubjectNameExceptionIsExactAndInternal(t *testing.T) {
	base := validEnvelopes()[EnvelopeKindNotice]
	for _, hash := range []string{"abc1234", strings.Repeat("a", 20) + strings.Repeat("1", 20)} {
		e := base
		e.Subject = "Escalation for " + hash
		if !hasProblem(ValidateEnvelope(e), "subject") {
			t.Fatalf("public validator accepted %q", e.Subject)
		}
		if err := NormalizeEnvelopePost(&PostMessageRequest{Envelope: &e}); err == nil {
			t.Fatalf("public normalization accepted %q", e.Subject)
		}
	}
	name := "builder-41b1c632"
	good := base
	good.Subject = "Owner attention: " + name + " is overdue on an assignment"
	if err := NormalizeBrokerPost(&PostMessageRequest{Envelope: &good}, name); err != nil {
		t.Fatalf("trusted exact name rejected: %v", err)
	}
	for _, subject := range []string{
		"Owner attention: " + name + " and abc1234 are overdue",
		"Owner attention: " + name + " and " + strings.Repeat("a", 20) + strings.Repeat("1", 20) + " are overdue",
		"Owner attention: " + name + " wrote to /tmp/report.json",
		"Owner attention: " + name + strings.Repeat("x", 120),
	} {
		e := base
		e.Subject = subject
		if err := NormalizeBrokerPost(&PostMessageRequest{Envelope: &e}, name); err == nil {
			t.Fatalf("broker normalization accepted unrelated invalid subject %q", subject)
		}
	}
	record := base
	record.Subject = "Owner attention: wi_74c050c73f7ff0b4 is overdue"
	if err := NormalizeBrokerPost(&PostMessageRequest{Envelope: &record}, "wi_74c050c73f7ff0b4"); err == nil {
		t.Fatal("broker normalization accepted a record ID as an agent name")
	}
	record.Subject = "Owner attention: wi_74c050c73f7ff0b4-41b1c632 is overdue"
	if err := NormalizeBrokerPost(&PostMessageRequest{Envelope: &record}, "wi_74c050c73f7ff0b4-41b1c632"); err == nil {
		t.Fatal("broker normalization hid a record ID in an agent name")
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

// Round-one blockers R3, R5, R6, R7 (Codex review of 118a602..53a4e1e).
func TestSortedKeysIsATotalOrder(t *testing.T) {
	keys := []string{"a1", "a01", "a2", "a10", "a1b", "b1", "a"}
	m := map[string]string{}
	for _, k := range keys {
		m[k] = k
	}
	first := sortedKeys(m)
	for i := 0; i < 200; i++ {
		if got := sortedKeys(m); !reflect.DeepEqual(got, first) {
			t.Fatalf("unstable order: %v vs %v", got, first)
		}
	}
	for _, a := range keys {
		for _, b := range keys {
			if a != b && naturalLess(a, b) == naturalLess(b, a) {
				t.Errorf("not antisymmetric: %s %s", a, b)
			}
			for _, c := range keys {
				if naturalLess(a, b) && naturalLess(b, c) && !naturalLess(a, c) {
					t.Errorf("not transitive: %s < %s < %s", a, b, c)
				}
			}
		}
	}
	e := Envelope{Kind: "assign", Subject: "Keys that compare equal numerically", Body: EnvelopeBody{Objective: "x", Owns: []string{"f"},
		Acceptance: map[string]string{"a1": "one", "a01": "zero one"}}}
	want := RenderText(e)
	for i := 0; i < 200; i++ {
		req := PostMessageRequest{Envelope: &e}
		if err := NormalizeEnvelopePost(&req); err != nil || req.Text != want {
			t.Fatalf("render diverged: %v %q", err, req.Text)
		}
		if err := NormalizeEnvelopePost(&req); err != nil {
			t.Fatalf("second normalize rejected its own rendering: %v", err)
		}
	}
}

func TestSemicolonsInsideValuesSurviveRoundTrip(t *testing.T) {
	e := Envelope{Kind: "result", Subject: "Both verification commands pass", Body: EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
		Evidence: map[string]Evidence{"e1": {Type: "command", Value: "go test ./...; ./verify.sh", Outcome: "ok; no warnings"}}}
	parsed, _ := ParseTextConvention(RenderText(e))
	if !reflect.DeepEqual(parsed.Evidence, e.Evidence) {
		t.Fatalf("evidence lost: %+v", parsed.Evidence)
	}
	a := Envelope{Kind: "assign", Subject: "Criteria that contain semicolons", Body: EnvelopeBody{Objective: "x", Owns: []string{"f"},
		Acceptance: map[string]string{"a1": "exits 2; prints an error", "a2": "tests pass"}}}
	parsed, _ = ParseTextConvention(RenderText(a))
	if !reflect.DeepEqual(parsed.Body.Acceptance, a.Body.Acceptance) {
		t.Fatalf("acceptance lost: %+v", parsed.Body.Acceptance)
	}
	key, item, ok := ParseEvidenceEntry("e1 (command): go test ./...; ./verify.sh -> ok")
	if !ok || key != "e1" || item != (Evidence{Type: "command", Value: "go test ./...; ./verify.sh", Outcome: "ok"}) {
		t.Fatalf("single entry %s %+v %v", key, item, ok)
	}
}

func TestSubjectRulesCatchPathsAndHashVariants(t *testing.T) {
	base := Envelope{Kind: "notice", Body: EnvelopeBody{Text: "x"}}
	for _, bad := range []string{"Write reports to /tmp", "Write reports to (/tmp/report.json) today", "Released candidate 43C1DA3 today",
		"Saved the order for WI_74C050C73F7FF0B4"} {
		e := base
		e.Subject = bad
		if !hasProblem(ValidateEnvelope(e), "subject") {
			t.Errorf("subject %q accepted", bad)
		}
	}
	for _, ok := range []string{"Handled 1500000 requests without errors", "Board and/or Teams share one layout", "Fixed the deadbeef cache path check"} {
		e := base
		e.Subject = ok
		if ps := ValidateEnvelope(e); ps != nil {
			t.Errorf("subject %q rejected: %v", ok, ps)
		}
	}
}

func TestQuestionWithURLIsOneQuestion(t *testing.T) {
	q := Envelope{Kind: "question", Subject: "Escaping rule for search links", Body: EnvelopeBody{Question: "Does /search?q=one need escaping?"}}
	if ps := ValidateEnvelope(q); ps != nil {
		t.Fatalf("rejected: %v", ps)
	}
}

// Round-one Fable blockers b4, b5 and follow-ups on subject false positives
// and the template's a1=pass fallback syntax.
func TestControlCharactersNameTheirField(t *testing.T) {
	e := validEnvelopes()[EnvelopeKindNotice]
	e.Body.Text = "bad \x01 char"
	if !hasProblem(ValidateEnvelope(e), "body.text") {
		t.Errorf("control char in body accepted: %v", ValidateEnvelope(e))
	}
	r := validEnvelopes()[EnvelopeKindResult]
	r.Evidence = map[string]Evidence{"e1": {Type: "command", Value: "go\x7ftest"}}
	if !hasProblem(ValidateEnvelope(r), "evidence.e1.value") {
		t.Errorf("control char in evidence accepted: %v", ValidateEnvelope(r))
	}
	long := validEnvelopes()[EnvelopeKindAssign]
	long.Body.Acceptance = map[string]string{}
	for i := 1; i <= 40; i++ {
		long.Body.Acceptance[fmt.Sprintf("a%d", i)] = strings.Repeat("x", 200)
	}
	long.Body.Objective = "x"
	if ps := ValidateEnvelope(long); !hasProblem(ps, "body") && !hasProblem(ps, "envelope") {
		t.Errorf("oversized rendering accepted")
	}
}

func TestRecipientIsValidated(t *testing.T) {
	for _, bad := range []string{"  ", "role:", "lead\nbuilder", "two words", "role:Lead Dev"} {
		e := validEnvelopes()[EnvelopeKindNotice]
		e.To = bad
		if !hasProblem(ValidateEnvelope(e), "to") {
			t.Errorf("to %q accepted", bad)
		}
	}
	for _, ok := range []string{"lead", "builder-2", "agt_0000000000000001", "role:reviewer", "role:db_handler"} {
		e := validEnvelopes()[EnvelopeKindNotice]
		e.To = ok
		if ps := ValidateEnvelope(e); ps != nil {
			t.Errorf("to %q rejected: %v", ok, ps)
		}
	}
}

func TestSubjectAllowsRoutesAndHexWords(t *testing.T) {
	for _, ok := range []string{"Add GET /v1/tasks/{id}/message-checks endpoint", "Deploy /state volume on the hub",
		"Rename the set_facade helper", "Backfill 10000000 rows overnight"} {
		e := Envelope{Kind: "notice", Subject: ok, Body: EnvelopeBody{Text: "x"}}
		if ps := ValidateEnvelope(e); ps != nil {
			t.Errorf("subject %q rejected: %v", ok, ps)
		}
	}
	for _, bad := range []string{"Wrote /srv/app/config.yaml today", "Report is in /home/me/out today", "Saved the order for wi_74c050c73f7ff0b4"} {
		e := Envelope{Kind: "notice", Subject: bad, Body: EnvelopeBody{Text: "x"}}
		if !hasProblem(ValidateEnvelope(e), "subject") {
			t.Errorf("subject %q accepted", bad)
		}
	}
}

func TestConventionAcceptsFlagStyleStatus(t *testing.T) {
	e, _ := ParseTextConvention("RESULT: Tests pass for the recipient check\nOutcome: done\nStatus: a1=pass, a2=fail\nEvidence: e1: go test -> ok")
	if e.Body.Status["a1"] != "pass" || e.Body.Status["a2"] != "fail" || ValidateEnvelope(e) != nil {
		t.Fatalf("flag-style status: %+v %v", e.Body.Status, ValidateEnvelope(e))
	}
}

// Round-two focused fix (Codex P2-P5).
func TestEscapedSemicolonsRoundTripExactly(t *testing.T) {
	e := Envelope{Kind: "assign", Subject: "Criteria with prose that looks like keys", Body: EnvelopeBody{Objective: "x", Owns: []string{"f"},
		Acceptance: map[string]string{"a1": "exits 2; note: stderr is empty", "a2": `path C:\tmp\; ok`}},
		Refs: map[string]string{"cmd": "make a; make b"}}
	parsed, _ := ParseTextConvention(RenderText(e))
	if !reflect.DeepEqual(parsed.Body.Acceptance, e.Body.Acceptance) || !reflect.DeepEqual(parsed.Refs, e.Refs) {
		t.Fatalf("round trip changed values: %+v %+v", parsed.Body.Acceptance, parsed.Refs)
	}
}

func TestHandWrittenEntriesSplitOnSemicolons(t *testing.T) {
	for _, line := range []string{"Acceptance: a1 exits 2; a2 prints an error", "Acceptance: a1=exits 2; a2=prints an error", "Acceptance: a1: exits 2; a2: prints an error"} {
		e, _ := ParseTextConvention("ASSIGN: Reject the empty recipient\nObjective: fail fast\nOwns: a.go\n" + line)
		if !reflect.DeepEqual(e.Body.Acceptance, map[string]string{"a1": "exits 2", "a2": "prints an error"}) {
			t.Errorf("%q parsed as %v", line, e.Body.Acceptance)
		}
	}
}

func TestRecipientMatchesAgentNameRule(t *testing.T) {
	for _, name := range []string{"_builder", "-reviewer", "a_b-c"} {
		if !ValidName(name) {
			t.Fatalf("fixture %q is not a valid agent name", name)
		}
		e := validEnvelopes()[EnvelopeKindNotice]
		e.To = name
		if ps := ValidateEnvelope(e); ps != nil {
			t.Errorf("to %q rejected: %v", name, ps)
		}
	}
}

func TestProblemOrderIsDeterministic(t *testing.T) {
	e := validEnvelopes()[EnvelopeKindNotice]
	e.Body.Acceptance = map[string]string{"a1": "bad \x01"}
	e.Body.Options = map[string]string{"o1": "bad \x01"}
	e.Body.Status = map[string]string{"a1": "pass\x01"}
	first := problemFields(ValidateEnvelope(e))
	for i := 0; i < 300; i++ {
		if got := problemFields(ValidateEnvelope(e)); !reflect.DeepEqual(got, first) {
			t.Fatalf("order changed: %v vs %v", got, first)
		}
	}
}

// Focused verification follow-up: escaping must round-trip without a semicolon too.
func TestBackslashValuesRoundTripWithoutSemicolons(t *testing.T) {
	e := Envelope{Kind: "result", Subject: "Windows path checks pass on the runner", Refs: map[string]string{"path": `C:\tmp`},
		Body:     EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
		Evidence: map[string]Evidence{"e1": {Type: "file", Value: `C:\tmp\out.txt`}}}
	parsed, _ := ParseTextConvention(RenderText(e))
	if parsed.Refs["path"] != `C:\tmp` || parsed.Evidence["e1"].Value != `C:\tmp\out.txt` {
		t.Fatalf("backslashes changed: %q %q", parsed.Refs["path"], parsed.Evidence["e1"].Value)
	}
	o, _ := ParseTextConvention("ASSIGN: Reject the empty recipient\nObjective: x\nOwns: C:\\\\src\\\\a.go, b.go\nAcceptance: a1: y")
	if !reflect.DeepEqual(o.Body.Owns, []string{`C:\\src\\a.go`, "b.go"}) {
		t.Fatalf("owns changed: %q", o.Body.Owns)
	}
}

func TestReviewConvergenceMetadataKindAndRendering(t *testing.T) {
	e := Envelope{Kind: "notice", Subject: "Exact review disposition saved", Body: EnvelopeBody{Text: "Saved"}, Review: &ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	if p := ValidateEnvelope(e); len(p) != 0 {
		t.Fatal(p)
	}
	if !strings.Contains(RenderText(e), "Review metadata:") {
		t.Fatal(RenderText(e))
	}
	e.Review.Mode = "focused"
	if len(ValidateEnvelope(e)) == 0 {
		t.Fatal("focused NOTICE accepted")
	}
	e.Review.Mode = "unknown"
	if len(ValidateEnvelope(e)) == 0 {
		t.Fatal("unknown review mode accepted")
	}
}

func TestReviewConvergenceReconciliationEnvelope(t *testing.T) {
	e := Envelope{Kind: "notice", Subject: "Reconcile exact legacy review history", Body: EnvelopeBody{Text: "Preserve source rounds"}, Review: &ReviewMetadata{Mode: "reconcile", LegacyRequests: []int64{17, 21}, Fix: "Source-linked round adoption"}, Evidence: map[string]Evidence{"e1": {Type: "record", Value: "Native legacy request records"}}}
	if p := ValidateEnvelope(e); len(p) != 0 {
		t.Fatal(p)
	}
	e.Kind = "request"
	e.Body.Ask = "Reconcile"
	if p := ValidateEnvelope(e); len(p) == 0 {
		t.Fatal("reconcile requires NOTICE")
	}
}

// The envelope title refusal repeats a finding ID through ReviewFindingIDEcho,
// the same cap and replacement the store's refusals use.
func TestEnvelopeReviewFindingTitleEchoesCappedID(t *testing.T) {
	exact := strings.Repeat("a", 64)
	for name, tc := range map[string]struct{ id, want string }{
		"normal":       {"f1", "f1"},
		"no ID":        {"", "0"},
		"at the cap":   {exact, exact},
		"over the cap": {exact + strings.Repeat("b", 40), exact + "\u2026"},
		"control":      {"f\x1b1\n\t2", "f 1  2"},
		"invalid utf8": {"f\xff1", "f 1"},
	} {
		t.Run(name, func(t *testing.T) {
			e := validEnvelopes()[EnvelopeKindResult]
			e.Review = &ReviewMetadata{Mode: "general", Findings: []ReviewFinding{{ID: tc.id, Title: ""}}}
			want := []Problem{{Field: "review.findings[" + tc.want + "].title", Reason: "must not be empty; a title is 1 to 120 characters"}}
			if got := ValidateEnvelope(e); !reflect.DeepEqual(got, want) {
				t.Fatalf("problems %q, want %q", got, want)
			}
		})
	}
}

func exitPtr(n int) *int { return &n }

func exitString(p *int) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprint(*p)
}

// Criterion a1: command evidence may carry an exit code read from a leading
// "exit N" in its outcome; anything else has none, and old entries stay valid.
func TestEvidenceExitParsedFromOutcome(t *testing.T) {
	for _, c := range []struct {
		entry string
		exit  *int
	}{
		{"e1 (command): go test ./x -> exit 0", exitPtr(0)},
		{"e1 (command): go test ./x -> exit 1", exitPtr(1)},
		{"e1: go test ./x -> exit 0; 62 passed", exitPtr(0)},
		{"e1: go test ./x -> EXIT 2; 1 failed", exitPtr(2)},
		{"e1: go test ./x -> exit 255", exitPtr(255)},
		{"e1: go test ./x -> ok", nil},
		{"e1: go test ./x -> ok (exit 0)", nil},
		{"e1: go test ./x -> exit unknown", nil},
		{"e1: go test ./x -> exiting early", nil},
		{"e1: go test ./x -> exit -1", nil},
		{"e1: go test ./x -> exit 300", nil},
		{"e1: go test ./x -> exit 1000", nil},
		{"e1 (record): saved row -> exit 1", nil},
		{"e1 (commit): abc1234", nil},
	} {
		_, ev, ok := ParseEvidenceEntry(c.entry)
		if !ok || exitString(ev.Exit) != exitString(c.exit) || exitString(EvidenceExit(ev)) != exitString(c.exit) {
			t.Errorf("%q: exit %s, want %s (ok %v)", c.entry, exitString(ev.Exit), exitString(c.exit), ok)
		}
	}
	// The outcome text is kept whole and round-trips through the rendered text.
	e := validEnvelopes()[EnvelopeKindResult]
	_, ev, _ := ParseEvidenceEntry("e1 (command): go test ./x -> exit 1; 2 failed")
	e.Evidence = map[string]Evidence{"e1": ev, "e2": {Type: "command", Value: "go vet ./...", Outcome: "ok"}}
	if ev.Outcome != "exit 1; 2 failed" || ValidateEnvelope(e) != nil {
		t.Fatalf("entry %+v problems %v", ev, ValidateEnvelope(e))
	}
	parsed, matched := ParseTextConvention(RenderText(e))
	if !matched || parsed.Evidence["e1"].Outcome != "exit 1; 2 failed" || exitString(parsed.Evidence["e1"].Exit) != "1" || parsed.Evidence["e2"].Exit != nil {
		t.Fatalf("round trip %+v", parsed.Evidence)
	}
}

// Criteria a2 and a3: the failed-command flag rule, and no flag without an exit.
func TestEvidenceFlagsForFailedCommand(t *testing.T) {
	cmd := func(outcome string) Evidence {
		return Evidence{Type: "command", Value: "go test ./x", Outcome: outcome}
	}
	for _, c := range []struct {
		name     string
		kind     string
		outcome  string
		status   map[string]string
		evidence map[string]Evidence
		want     []EnvelopeFlag
	}{
		{"done with exit 1", "result", "done", map[string]string{"a1": "fail"}, map[string]Evidence{"e1": cmd("exit 1; 2 failed")},
			[]EnvelopeFlag{{Kind: "failed_command", Field: "evidence.e1", Reason: "e1 exited 1, but this result reports done"}}},
		{"partial but every status pass", "result", "partial", map[string]string{"a1": "pass", "a2": "pass"}, map[string]Evidence{"e1": cmd("exit 2")},
			[]EnvelopeFlag{{Kind: "failed_command", Field: "evidence.e1", Reason: "e1 exited 2, but this result reports every status pass"}}},
		{"exit 0", "result", "done", map[string]string{"a1": "pass"}, map[string]Evidence{"e1": cmd("exit 0; ok")}, nil},
		{"missing exit", "result", "done", map[string]string{"a1": "pass"}, map[string]Evidence{"e1": cmd("failed")}, nil},
		{"honest failure", "result", "partial", map[string]string{"a1": "fail"}, map[string]Evidence{"e1": cmd("exit 1")}, nil},
		{"record type", "result", "done", map[string]string{"a1": "pass"}, map[string]Evidence{"e1": {Type: "record", Value: "row", Outcome: "exit 1"}}, nil},
		{"finding kind", "finding", "", nil, map[string]Evidence{"e1": cmd("exit 1")}, nil},
		{"two failed entries in key order", "result", "done", map[string]string{"a1": "pass"},
			map[string]Evidence{"e10": cmd("exit 3"), "e2": cmd("exit 1"), "e1": cmd("exit 0")},
			[]EnvelopeFlag{{Kind: "failed_command", Field: "evidence.e2", Reason: "e2 exited 1, but this result reports done"},
				{Kind: "failed_command", Field: "evidence.e10", Reason: "e10 exited 3, but this result reports done"}}},
	} {
		e := Envelope{Kind: c.kind, Subject: "Fixture for the failed command flag", Body: EnvelopeBody{Outcome: c.outcome, Status: c.status, Severity: "low", Summary: "s"}, Evidence: c.evidence}
		DeriveEvidence(&e)
		if !reflect.DeepEqual(e.Flags, c.want) {
			t.Errorf("%s: flags %+v, want %+v", c.name, e.Flags, c.want)
		}
		for k, ev := range e.Evidence {
			if exitString(ev.Exit) != exitString(EvidenceExit(ev)) {
				t.Errorf("%s: %s exit %s", c.name, k, exitString(ev.Exit))
			}
		}
	}
}

// Criterion a2: the hub accepts a flagged result (nothing is refused), stores
// the derived exit and flag, keeps old clients' text, and replaces forged values.
func TestNormalizeEnvelopePostFlagsWithoutRefusing(t *testing.T) {
	failed := func() *Envelope {
		return &Envelope{Kind: "result", Subject: "Tests pass for the flag fixture", Body: EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
			Evidence: map[string]Evidence{"e1": {Type: "command", Value: "go test ./x", Outcome: "exit 1; 2 failed"}, "e2": {Type: "command", Value: "go vet ./...", Outcome: "ok"}}}
	}
	// An old tt sends no exit field and the text it rendered itself.
	req := PostMessageRequest{Envelope: failed()}
	req.Text = RenderText(*req.Envelope)
	oldText := req.Text
	if err := NormalizeEnvelopePost(&req); err != nil {
		t.Fatalf("flagged result refused: %v", err)
	}
	if req.Text != oldText || exitString(req.Envelope.Evidence["e1"].Exit) != "1" || req.Envelope.Evidence["e2"].Exit != nil ||
		len(req.Envelope.Flags) != 1 || req.Envelope.Flags[0].Field != "evidence.e1" || !strings.Contains(req.Envelope.Flags[0].Reason, "e1") {
		t.Fatalf("normalized %+v text %q", req.Envelope, req.Text)
	}
	// Forged values: a clean result's flags are cleared, a wrong exit is rederived.
	clean := failed()
	clean.Evidence = map[string]Evidence{"e1": {Type: "command", Value: "go test ./x", Outcome: "exit 0", Exit: exitPtr(1)}}
	clean.Flags = []EnvelopeFlag{{Kind: "failed_command", Field: "evidence.e1", Reason: "forged"}}
	req = PostMessageRequest{Envelope: clean}
	if err := NormalizeEnvelopePost(&req); err != nil || req.Envelope.Flags != nil || exitString(req.Envelope.Evidence["e1"].Exit) != "0" {
		t.Fatalf("forged clean: %v %+v", err, req.Envelope)
	}
	hidden := failed()
	hidden.Evidence["e1"] = Evidence{Type: "command", Value: "go test ./x", Outcome: "exit 1", Exit: exitPtr(0)}
	req = PostMessageRequest{Envelope: hidden}
	if err := NormalizeEnvelopePost(&req); err != nil || len(req.Envelope.Flags) != 1 || exitString(req.Envelope.Evidence["e1"].Exit) != "1" {
		t.Fatalf("hidden failure: %v %+v", err, req.Envelope)
	}
}

// The 8 KB envelope limit measures what the sender wrote; derived exit and
// flags never push a message over it, on either normalize pass.
func TestEnvelopeSizeLimitExcludesDerivedFields(t *testing.T) {
	build := func(pad int) Envelope {
		e := Envelope{Kind: "result", Subject: "Tests pass for the size limit fixture", Body: EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
			Evidence: map[string]Evidence{"e1": {Type: "command", Value: "go test ./x", Outcome: "exit 1"}}}
		for i := 0; pad > 0; i++ {
			n := pad
			if n > 1500 {
				n = 1500
			}
			e.Attachments = append(e.Attachments, strings.Repeat("a", n))
			pad -= n
		}
		return e
	}
	size := func(e Envelope) int { b, _ := json.Marshal(WithoutDerived(e)); return len(b) }
	pad := 0
	for size(build(pad)) < MaxEnvelopeBytes {
		pad++
	}
	// Each extra attachment adds separator bytes; step down to the exact limit.
	for size(build(pad)) > MaxEnvelopeBytes {
		pad--
	}
	e := build(pad)
	if size(e) != MaxEnvelopeBytes {
		t.Fatalf("fixture is %d bytes, want exactly %d", size(e), MaxEnvelopeBytes)
	}
	if len(RenderText(e)) > MaxTextLen {
		t.Fatalf("fixture renders to %d bytes", len(RenderText(e)))
	}
	if p := ValidateEnvelope(e); p != nil {
		t.Fatalf("at the limit: %v", p)
	}
	env := e
	req := PostMessageRequest{Envelope: &env}
	if err := NormalizeEnvelopePost(&req); err != nil {
		t.Fatalf("first normalize: %v", err)
	}
	if whole, _ := json.Marshal(*req.Envelope); len(whole) <= MaxEnvelopeBytes {
		t.Fatalf("derived envelope is %d bytes; the fixture should exceed the limit only with derived fields", len(whole))
	}
	if err := NormalizeEnvelopePost(&req); err != nil || len(req.Envelope.Flags) != 1 {
		t.Fatalf("second normalize: %v %+v", err, req.Envelope.Flags)
	}
	if p := ValidateEnvelope(*req.Envelope); p != nil {
		t.Fatalf("derived envelope: %v", p)
	}
	over := build(pad + 1)
	if size(over) != MaxEnvelopeBytes+1 {
		t.Fatalf("over fixture is %d bytes", size(over))
	}
	if p := ValidateEnvelope(over); len(p) != 1 || p[0].String() != fmt.Sprintf("envelope: must be at most %d bytes", MaxEnvelopeBytes) {
		t.Fatalf("one byte over: %v", p)
	}
}

func TestEnvelopeWithoutDerivedLeavesInputUnchanged(t *testing.T) {
	e := Envelope{Kind: "result", Evidence: map[string]Evidence{"e1": {Type: "command", Value: "x", Outcome: "exit 1", Exit: exitPtr(1)}},
		Flags: []EnvelopeFlag{{Kind: "failed_command", Field: "evidence.e1", Reason: "r"}}}
	plain := WithoutDerived(e)
	if plain.Flags != nil || plain.Evidence["e1"].Exit != nil || plain.Evidence["e1"].Outcome != "exit 1" {
		t.Fatalf("plain %+v", plain)
	}
	if len(e.Flags) != 1 || exitString(e.Evidence["e1"].Exit) != "1" {
		t.Fatalf("input changed %+v", e)
	}
}
