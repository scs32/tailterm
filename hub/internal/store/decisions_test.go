package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func decisionFixture(t *testing.T) (*Store, context.Context, api.Caller, api.Task, api.Agent) {
	t.Helper()
	s, ctx, by := workItemStore(t)
	task, author := workItemProject(t, s, ctx, by, "Synthetic decisions", "worker")
	return s, ctx, by, task, author
}

func decisionRequest(author api.Agent, key string) api.CreateDecisionRequest {
	return api.CreateDecisionRequest{
		DecisionRequest: api.DecisionRequest{
			Question: "Which rollout should I implement?",
			Options: []api.DecisionOption{
				{ID: "staged", Label: "Staged rollout", Description: "Verify with a small group first."},
				{ID: "all", Label: "Everyone at once", Description: "Enable it for every user after testing."},
			},
			RecommendedOptionID:  "staged",
			RecommendationReason: "It limits the impact of a problem.",
		},
		AgentID: author.ID, RequestID: key,
	}
}

func countDecisionRows(t *testing.T, s *Store) (messages, requests, answers, events, receipts, links int) {
	t.Helper()
	err := s.db.QueryRow(`SELECT
(SELECT count(*) FROM messages),(SELECT count(*) FROM decision_requests),
(SELECT count(*) FROM decision_answers),(SELECT count(*) FROM events),
(SELECT count(*) FROM message_post_requests),(SELECT count(*) FROM message_work_item_links)`).Scan(
		&messages, &requests, &answers, &events, &receipts, &links,
	)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestDecisionsRoundTripReplayProjectionAndLifecycle(t *testing.T) {
	s, ctx, by, task, author := decisionFixture(t)
	item := createWorkItem(t, s, ctx, by, task, "decision-item")
	order, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Implement the approved direction"}, by)
	if err != nil {
		t.Fatal(err)
	}
	req := decisionRequest(author, "decision-roundtrip")
	req.WorkItems = []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	req.WorkOrderMessage = &api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	created, err := s.CreateDecision(ctx, task.ID, req, by)
	if err != nil {
		t.Fatal(err)
	}
	if created.DecisionRequest == nil || !reflect.DeepEqual(*created.DecisionRequest, req.DecisionRequest) || created.From.AgentID != author.ID || created.To != "" || created.Text != api.FormatDecisionRequest(req.DecisionRequest) {
		t.Fatalf("created decision changed: %+v", created)
	}
	if created.PostReceipt == nil || created.PostReceipt.RequestID != req.RequestID || !reflect.DeepEqual(created.WorkItems, req.WorkItems) || !reflect.DeepEqual(created.WorkOrderMessage, req.WorkOrderMessage) {
		t.Fatalf("created decision lost audit context: %+v", created)
	}

	updatedTitle := "Edited after the question"
	if _, err = s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Title: &updatedTitle}, by); err != nil {
		t.Fatal(err)
	}
	beforeReplay := [6]int{}
	beforeReplay[0], beforeReplay[1], beforeReplay[2], beforeReplay[3], beforeReplay[4], beforeReplay[5] = countDecisionRows(t, s)
	replayed, err := s.CreateDecision(ctx, task.ID, req, by)
	if err != nil || !reflect.DeepEqual(created, replayed) {
		t.Fatalf("item-edit replay changed: %+v %+v %v", created, replayed, err)
	}
	afterReplay := [6]int{}
	afterReplay[0], afterReplay[1], afterReplay[2], afterReplay[3], afterReplay[4], afterReplay[5] = countDecisionRows(t, s)
	if beforeReplay != afterReplay {
		t.Fatalf("create replay emitted a write: before=%v after=%v", beforeReplay, afterReplay)
	}
	changed := req
	changed.Question = "A different question using the same key"
	if _, err = s.CreateDecision(ctx, task.ID, changed, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed create retry error = %v", err)
	}

	if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: author.ID, RunID: author.RunID}, by); err != nil {
		t.Fatal(err)
	}
	retired := api.AgentRetired
	if _, err = s.UpdateAgent(ctx, author.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
		t.Fatal(err)
	}
	lifecycleReplay, err := s.CreateDecision(ctx, task.ID, req, by)
	if err != nil || !reflect.DeepEqual(lifecycleReplay, created) {
		t.Fatalf("lifecycle replay changed: %+v, %v", lifecycleReplay, err)
	}
	if stillRetired, err := s.GetAgent(ctx, author.ID); err != nil || stillRetired.Status != api.AgentRetired {
		t.Fatalf("request replay changed lifecycle: %+v, %v", stillRetired, err)
	}
	answerReq := api.AnswerDecisionRequest{RequestID: "answer-roundtrip", OptionID: "staged", Text: "Proceed with the small group."}
	answer, err := s.AnswerDecision(ctx, task.ID, created.Seq, answerReq, by)
	if err != nil {
		t.Fatal(err)
	}
	wantAnswer := api.DecisionAnswer{RequestSeq: created.Seq, OptionID: answerReq.OptionID, Text: answerReq.Text}
	if answer.DecisionAnswer == nil || *answer.DecisionAnswer != wantAnswer || answer.From.AgentID != "" || answer.To != author.ID || answer.ReplyTo != created.Seq || answer.Text != api.FormatDecisionAnswer(created.Seq, answerReq, req.DecisionRequest) {
		t.Fatalf("answer changed: %+v", answer)
	}
	resumed, err := s.GetAgent(ctx, author.ID)
	if err != nil || resumed.Status != api.AgentDone || resumed.RunID != author.RunID {
		t.Fatalf("human answer did not use existing resume semantics: %+v %v", resumed, err)
	}
	var resumeEvents int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE task_id=? AND agent_id=? AND kind=?`, task.ID, author.ID, api.EventResumed).Scan(&resumeEvents); err != nil || resumeEvents != 1 {
		t.Fatalf("resume event count = %d, %v", resumeEvents, err)
	}
	if _, err = s.AnswerDecision(ctx, task.ID, created.Seq, api.AnswerDecisionRequest{RequestID: "second-answer", OptionID: "all"}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("second answer error = %v", err)
	}

	list, err := s.ListDecisions(ctx, task.ID, 0, 100)
	if err != nil || len(list.Decisions) != 1 || !reflect.DeepEqual(list.Decisions[0].Request, created) || list.Decisions[0].Answer == nil || !reflect.DeepEqual(*list.Decisions[0].Answer, answer) {
		t.Fatalf("decision projection = %+v, %v", list, err)
	}
	messages, err := s.ListMessages(ctx, task.ID, 0, "", 100)
	if err != nil || len(messages) != 3 || !reflect.DeepEqual(messages[1], created) || !reflect.DeepEqual(messages[2], answer) {
		t.Fatalf("immutable message projection = %+v, %v", messages, err)
	}

	if _, err = s.CloseTask(ctx, task.ID, by); err != nil {
		t.Fatal(err)
	}
	beforeClosedReplay := [6]int{}
	beforeClosedReplay[0], beforeClosedReplay[1], beforeClosedReplay[2], beforeClosedReplay[3], beforeClosedReplay[4], beforeClosedReplay[5] = countDecisionRows(t, s)
	createAfterClose, err := s.CreateDecision(ctx, task.ID, req, by)
	if err != nil || !reflect.DeepEqual(createAfterClose, created) {
		t.Fatalf("create replay after closure = %+v, %v", createAfterClose, err)
	}
	answerAfterClose, err := s.AnswerDecision(ctx, task.ID, created.Seq, answerReq, by)
	if err != nil || !reflect.DeepEqual(answerAfterClose, answer) {
		t.Fatalf("answer replay after closure = %+v, %v", answerAfterClose, err)
	}
	afterClosedReplay := [6]int{}
	afterClosedReplay[0], afterClosedReplay[1], afterClosedReplay[2], afterClosedReplay[3], afterClosedReplay[4], afterClosedReplay[5] = countDecisionRows(t, s)
	if beforeClosedReplay != afterClosedReplay {
		t.Fatalf("closed replay emitted a write: before=%v after=%v", beforeClosedReplay, afterClosedReplay)
	}
	if closedList, err := s.ListDecisions(ctx, task.ID, 0, 100); err != nil || len(closedList.Decisions) != 1 {
		t.Fatalf("closed decision history = %+v, %v", closedList, err)
	}
	if recovered, err := s.GetMessagePostReceipt(ctx, task.ID, req.RequestID, author.ID, by); err != nil || !reflect.DeepEqual(recovered, created) {
		t.Fatalf("request receipt recovery = %+v, %v", recovered, err)
	}
	if recovered, err := s.GetMessagePostReceipt(ctx, task.ID, answerReq.RequestID, "", by); err != nil || !reflect.DeepEqual(recovered, answer) {
		t.Fatalf("answer receipt recovery = %+v, %v", recovered, err)
	}
}

func TestDecisionPaginationAndOrdinaryRepliesDoNotResolve(t *testing.T) {
	s, ctx, by, task, author := decisionFixture(t)
	created := make([]api.Message, 3)
	for i, key := range []string{"page-a", "page-b", "page-c"} {
		req := decisionRequest(author, key)
		req.Question += " " + key
		var err error
		created[i], err = s.CreateDecision(ctx, task.ID, req, by)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "An ordinary reply", To: author.ID, ReplyTo: created[0].Seq}, by); err != nil {
		t.Fatal(err)
	}
	first, err := s.ListDecisions(ctx, task.ID, 0, 2)
	if err != nil || len(first.Decisions) != 2 || first.NextAfter != created[1].Seq || first.Decisions[0].Answer != nil {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	second, err := s.ListDecisions(ctx, task.ID, first.NextAfter, 2)
	if err != nil || len(second.Decisions) != 1 || second.NextAfter != 0 || second.Decisions[0].Request.Seq != created[2].Seq {
		t.Fatalf("second page = %+v, %v", second, err)
	}
	if _, err = s.ListDecisions(ctx, task.ID, 0, api.MaxDecisionPage+1); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("oversized decision page error = %v", err)
	}
}

func TestConcurrentDecisionRetriesAndFirstAnswerWins(t *testing.T) {
	s, ctx, by, task, author := decisionFixture(t)
	request := decisionRequest(author, "concurrent-create")
	const count = 8
	created := make([]api.Message, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range created {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			created[i], errs[i] = s.CreateDecision(ctx, task.ID, request, by)
		}(i)
	}
	wg.Wait()
	for i := range created {
		if errs[i] != nil || created[i].Seq != created[0].Seq {
			t.Fatalf("create retry %d = %+v, %v", i, created[i], errs[i])
		}
	}

	answer := api.AnswerDecisionRequest{RequestID: "concurrent-answer", OptionID: "staged"}
	answers := make([]api.Message, count)
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			answers[i], errs[i] = s.AnswerDecision(ctx, task.ID, created[0].Seq, answer, by)
		}(i)
	}
	wg.Wait()
	for i := range answers {
		if errs[i] != nil || answers[i].Seq != answers[0].Seq {
			t.Fatalf("answer retry %d = %+v, %v", i, answers[i], errs[i])
		}
	}
	_, requests, answerRows, _, receipts, _ := countDecisionRows(t, s)
	if requests != 1 || answerRows != 1 || receipts != 2 {
		t.Fatalf("duplicate rows: requests=%d answers=%d receipts=%d", requests, answerRows, receipts)
	}

	otherRequest := decisionRequest(author, "concurrent-winner-request")
	other, err := s.CreateDecision(ctx, task.ID, otherRequest, by)
	if err != nil {
		t.Fatal(err)
	}
	results := make([]error, count)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			option := "staged"
			if i%2 == 1 {
				option = "all"
			}
			_, results[i] = s.AnswerDecision(ctx, task.ID, other.Seq, api.AnswerDecisionRequest{RequestID: "winner-key-" + string(rune('a'+i)), OptionID: option}, by)
		}(i)
	}
	wg.Wait()
	succeeded, conflicted := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, api.ErrConflict):
			conflicted++
		default:
			t.Fatalf("unexpected concurrent answer error: %v", err)
		}
	}
	if succeeded != 1 || conflicted != count-1 {
		t.Fatalf("concurrent winners=%d conflicts=%d", succeeded, conflicted)
	}
}

func TestDecisionCreateAndAnswerFailuresRollbackAtomically(t *testing.T) {
	createTriggers := []struct{ name, trigger string }{
		{"message event", `CREATE TRIGGER fail_decision_event BEFORE INSERT ON events WHEN NEW.kind='message' BEGIN SELECT RAISE(ABORT,'event failed'); END`},
		{"request metadata", `CREATE TRIGGER fail_decision_request BEFORE INSERT ON decision_requests BEGIN SELECT RAISE(ABORT,'request failed'); END`},
		{"receipt", `CREATE TRIGGER fail_decision_receipt BEFORE INSERT ON message_post_requests BEGIN SELECT RAISE(ABORT,'receipt failed'); END`},
	}
	for _, tc := range createTriggers {
		t.Run("create "+tc.name, func(t *testing.T) {
			s, ctx, by, task, author := decisionFixture(t)
			item := createWorkItem(t, s, ctx, by, task, "rollback-item")
			req := decisionRequest(author, "rollback-create")
			req.WorkItems = []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
			before := [6]int{}
			before[0], before[1], before[2], before[3], before[4], before[5] = countDecisionRows(t, s)
			if _, err := s.db.Exec(tc.trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateDecision(ctx, task.ID, req, by); err == nil {
				t.Fatal("injected create failure committed")
			}
			after := [6]int{}
			after[0], after[1], after[2], after[3], after[4], after[5] = countDecisionRows(t, s)
			if before != after {
				t.Fatalf("partial create: before=%v after=%v", before, after)
			}
		})
	}

	answerTriggers := []struct{ name, trigger string }{
		{"answer metadata", `CREATE TRIGGER fail_decision_answer BEFORE INSERT ON decision_answers BEGIN SELECT RAISE(ABORT,'answer failed'); END`},
		{"resume event", `CREATE TRIGGER fail_decision_resume BEFORE INSERT ON events WHEN NEW.kind='resumed' BEGIN SELECT RAISE(ABORT,'resume failed'); END`},
		{"receipt", `CREATE TRIGGER fail_answer_receipt BEFORE INSERT ON message_post_requests WHEN NEW.agent_id='' BEGIN SELECT RAISE(ABORT,'receipt failed'); END`},
	}
	for _, tc := range answerTriggers {
		t.Run("answer "+tc.name, func(t *testing.T) {
			s, ctx, by, task, author := decisionFixture(t)
			created, err := s.CreateDecision(ctx, task.ID, decisionRequest(author, "rollback-question"), by)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: author.ID, RunID: author.RunID}, by); err != nil {
				t.Fatal(err)
			}
			retired := api.AgentRetired
			if _, err = s.UpdateAgent(ctx, author.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
				t.Fatal(err)
			}
			before := [6]int{}
			before[0], before[1], before[2], before[3], before[4], before[5] = countDecisionRows(t, s)
			if _, err = s.db.Exec(tc.trigger); err != nil {
				t.Fatal(err)
			}
			if _, err = s.AnswerDecision(ctx, task.ID, created.Seq, api.AnswerDecisionRequest{RequestID: "rollback-answer", OptionID: "staged"}, by); err == nil {
				t.Fatal("injected answer failure committed")
			}
			after := [6]int{}
			after[0], after[1], after[2], after[3], after[4], after[5] = countDecisionRows(t, s)
			if before != after {
				t.Fatalf("partial answer: before=%v after=%v", before, after)
			}
			persisted, err := s.GetAgent(ctx, author.ID)
			if err != nil || persisted.Status != api.AgentRetired {
				t.Fatalf("failed answer changed lifecycle: %+v %v", persisted, err)
			}
		})
	}
}

func TestDecisionValidationAndCrossOperationReceiptConflicts(t *testing.T) {
	s, ctx, by, task, author := decisionFixture(t)
	other, otherAuthor := workItemProject(t, s, ctx, by, "Other decisions", "other")
	request := decisionRequest(author, "shared-key")
	if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: author.ID, RequestID: request.RequestID, Text: "ordinary"}, by); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDecision(ctx, task.ID, request, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("ordinary key replayed as decision: %v", err)
	}
	request.RequestID = "wrong-agent"
	request.AgentID = otherAuthor.ID
	if _, err := s.CreateDecision(ctx, task.ID, request, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("foreign author error = %v", err)
	}
	request = decisionRequest(author, "foreign-item")
	foreignItem := createWorkItem(t, s, ctx, by, other, "foreign-item")
	request.WorkItems = []api.MessageWorkItem{{ItemTaskID: other.ID, ItemID: foreignItem.ID, ItemRevision: foreignItem.Revision, Relationship: "primary"}}
	if _, err := s.CreateDecision(ctx, task.ID, request, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("foreign context error = %v", err)
	}
	inactive, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "inactive", Host: "host", Session: "inactive", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	exited := api.AgentExited
	if _, err = s.UpdateAgent(ctx, inactive.ID, api.UpdateAgentRequest{Status: &exited}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateDecision(ctx, task.ID, decisionRequest(inactive, "inactive-author"), by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("inactive author error = %v", err)
	}

	valid, err := s.CreateDecision(ctx, task.ID, decisionRequest(author, "answer-validation"), by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AnswerDecision(ctx, task.ID, valid.Seq, api.AnswerDecisionRequest{RequestID: "agent-answer", AgentID: author.ID, OptionID: "staged"}, by); !errors.Is(err, ErrDecisionAnswerForbidden) {
		t.Fatalf("agent answer error = %v", err)
	}
	if _, err = s.AnswerDecision(ctx, task.ID, valid.Seq, api.AnswerDecisionRequest{RequestID: "invalid-option", OptionID: "missing"}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("invalid option error = %v", err)
	}
	if _, err = s.AnswerDecision(ctx, task.ID, valid.Seq, api.AnswerDecisionRequest{RequestID: "blank-custom", Text: "  "}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("blank custom error = %v", err)
	}
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{RequestID: "answer-operation-key", Text: "ordinary human message"}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AnswerDecision(ctx, task.ID, valid.Seq, api.AnswerDecisionRequest{RequestID: "answer-operation-key", OptionID: "staged"}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("ordinary key replayed as answer: %v", err)
	}
	if _, err = s.AnswerDecision(ctx, task.ID, 999999, api.AnswerDecisionRequest{RequestID: "missing-question", Text: "custom"}, by); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("missing question error = %v", err)
	}
}

func TestDecisionMigrationPreservesA1ReceiptAndHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pre-decisions.sqlite")
	by := api.Caller{Node: "migration-node", User: "owner"}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, author := workItemProject(t, s, ctx, by, "Migration decisions", "worker")
	ordinaryReq := api.PostMessageRequest{AgentID: author.ID, RequestID: "ordinary-before-decisions", Text: "Existing A1 receipt"}
	ordinary, err := s.PostMessage(ctx, task.ID, ordinaryReq, by)
	if err != nil {
		t.Fatal(err)
	}
	var oldHash string
	if err = s.db.QueryRow(`SELECT payload_hash FROM message_post_requests WHERE message_seq=?`, ordinary.Seq).Scan(&oldHash); err != nil {
		t.Fatal(err)
	}
	if oldHash != requestHash(ordinaryReq) {
		t.Fatalf("ordinary hash shape changed before migration: %q", oldHash)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TABLE decision_answers; DROP TABLE decision_requests;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	replayed, err := s.PostMessage(ctx, task.ID, ordinaryReq, by)
	if err != nil || !reflect.DeepEqual(replayed, ordinary) {
		t.Fatalf("pre-decision A1 replay = %+v, %v", replayed, err)
	}
	created, err := s.CreateDecision(ctx, task.ID, decisionRequest(author, "after-migration"), by)
	if err != nil {
		t.Fatal(err)
	}
	answered, err := s.AnswerDecision(ctx, task.ID, created.Seq, api.AnswerDecisionRequest{RequestID: "after-migration-answer", Text: "Use a custom plan."}, by)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListDecisions(ctx, task.ID, 0, 100)
	if err != nil || len(list.Decisions) != 1 || !reflect.DeepEqual(list.Decisions[0].Request, created) || list.Decisions[0].Answer == nil || !reflect.DeepEqual(*list.Decisions[0].Answer, answered) {
		t.Fatalf("reopened decision history = %+v, %v", list, err)
	}
	var violations int
	if err = s.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign-key integrity = %d, %v", violations, err)
	}
}
