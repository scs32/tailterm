package server

import (
	"strconv"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func httpDecisionRequest(author api.Agent, key string) api.CreateDecisionRequest {
	return api.CreateDecisionRequest{
		DecisionRequest: api.DecisionRequest{
			Question: "Which path should the synthetic test use?",
			Options: []api.DecisionOption{
				{ID: "safe", Label: "Safe path", Description: "Use the smaller rollout."},
				{ID: "fast", Label: "Fast path", Description: "Use the broad rollout."},
			},
			RecommendedOptionID: "safe", RecommendationReason: "It bounds the test impact.",
		},
		AgentID: author.ID, RequestID: key,
	}
}

func TestDecisionHTTPRoutesReplayPaginationAndClosure(t *testing.T) {
	c := newClient(t)
	task := c.task("decision routes")
	author := c.agent(task, "requester")
	firstReq := httpDecisionRequest(author, "http-first")
	var first, replay api.Message
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/decisions", firstReq, &first); code != 201 || first.DecisionRequest == nil || first.PostReceipt == nil {
		t.Fatalf("create decision = %d %+v", code, first)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/decisions", firstReq, &replay); code != 201 || replay.Seq != first.Seq || replay.PostReceipt.ID != first.PostReceipt.ID {
		t.Fatalf("create replay = %d %+v", code, replay)
	}
	secondReq := httpDecisionRequest(author, "http-second")
	secondReq.Question = "A second synthetic question?"
	var second api.Message
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/decisions", secondReq, &second); code != 201 {
		t.Fatalf("second decision = %d", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{ReplyTo: first.Seq, To: author.ID, Text: "ordinary reply"}, nil); code != 201 {
		t.Fatalf("ordinary reply = %d", code)
	}
	var page api.DecisionList
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/decisions?after=0&limit=1", nil, &page); code != 200 || len(page.Decisions) != 1 || page.Decisions[0].Answer != nil || page.NextAfter != first.Seq {
		t.Fatalf("first decision page = %d %+v", code, page)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/decisions?after=bad&limit=1", nil, nil); code != 400 {
		t.Fatalf("malformed cursor = %d", code)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/decisions?limit=0", nil, nil); code != 400 {
		t.Fatalf("zero limit = %d", code)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/decisions?limit=101", nil, nil); code != 400 {
		t.Fatalf("oversized limit = %d", code)
	}

	answerReq := api.AnswerDecisionRequest{RequestID: "http-answer", OptionID: "safe", Text: "Proceed."}
	var answer api.Message
	answerPath := "/v1/tasks/" + task.ID + "/decisions/" + strconv.FormatInt(first.Seq, 10) + "/answer"
	if code := c.do("POST", answerPath, answerReq, &answer); code != 201 || answer.DecisionAnswer == nil || answer.To != author.ID || answer.ReplyTo != first.Seq || answer.PostReceipt == nil {
		t.Fatalf("answer decision = %d %+v", code, answer)
	}
	if code := c.do("POST", answerPath, answerReq, &replay); code != 201 || replay.Seq != answer.Seq || replay.PostReceipt.ID != answer.PostReceipt.ID {
		t.Fatalf("answer replay = %d %+v", code, replay)
	}
	agentAnswer := api.AnswerDecisionRequest{RequestID: "agent-http-answer", AgentID: author.ID, OptionID: "safe"}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/decisions/"+strconv.FormatInt(second.Seq, 10)+"/answer", agentAnswer, nil); code != 403 {
		t.Fatalf("agent-authored answer = %d", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/decisions/not-a-seq/answer", answerReq, nil); code != 400 {
		t.Fatalf("malformed request seq = %d", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/decisions/0/answer", api.AnswerDecisionRequest{RequestID: "zero-seq", Text: "custom"}, nil); code != 400 {
		t.Fatalf("nonpositive request seq = %d", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/decisions/999999/answer", api.AnswerDecisionRequest{RequestID: "missing-seq", Text: "custom"}, nil); code != 404 {
		t.Fatalf("missing request seq = %d", code)
	}
	var recovered api.Message
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/messages/receipts/"+firstReq.RequestID+"?agentId="+author.ID, nil, &recovered); code != 200 || recovered.DecisionRequest == nil || recovered.Seq != first.Seq {
		t.Fatalf("request recovery = %d %+v", code, recovered)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/messages/receipts/"+answerReq.RequestID, nil, &recovered); code != 200 || recovered.DecisionAnswer == nil || recovered.Seq != answer.Seq {
		t.Fatalf("answer recovery = %d %+v", code, recovered)
	}

	if code := c.do("DELETE", "/v1/tasks/"+task.ID, nil, nil); code != 200 {
		t.Fatalf("close task = %d", code)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/decisions", nil, &page); code != 200 || len(page.Decisions) != 2 || page.Decisions[0].Answer == nil {
		t.Fatalf("closed history = %d %+v", code, page)
	}
	if code := c.do("POST", answerPath, answerReq, &replay); code != 201 || replay.Seq != answer.Seq {
		t.Fatalf("closed answer replay = %d %+v", code, replay)
	}
	thirdReq := httpDecisionRequest(author, "closed-create")
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/decisions", thirdReq, nil); code != 409 {
		t.Fatalf("closed create = %d", code)
	}
}
