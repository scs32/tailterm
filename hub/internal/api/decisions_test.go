package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDecisionClientReadsEscapedAnsweredPage(t *testing.T) {
	question := DecisionRequest{Question: strings.Repeat("<", 2000), RecommendationReason: strings.Repeat("<", 100), RecommendedOptionID: "first"}
	for _, id := range []string{"first", "second", "third", "fourth", "fifth"} {
		question.Options = append(question.Options, DecisionOption{ID: id, Label: strings.Repeat("<", 120), Description: strings.Repeat("<", 900)})
	}
	if err := ValidateDecisionRequest(question); err != nil {
		t.Fatal(err)
	}
	answer := AnswerDecisionRequest{OptionID: "first", Text: strings.Repeat("<", 4000)}
	if err := ValidateDecisionAnswer(answer, question); err != nil {
		t.Fatal(err)
	}
	page := DecisionList{Decisions: make([]DecisionRecord, MaxDecisionPage)}
	for i := range page.Decisions {
		seq := int64(i*2 + 1)
		page.Decisions[i] = DecisionRecord{
			Request: Message{Seq: seq, Text: FormatDecisionRequest(question), DecisionRequest: &question},
			Answer:  &Message{Seq: seq + 1, Text: FormatDecisionAnswer(seq, answer, question), DecisionAnswer: &DecisionAnswer{RequestSeq: seq, OptionID: answer.OptionID, Text: answer.Text}},
		}
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= 4<<20 || len(encoded) > 16<<20 {
		t.Fatalf("fixture must exercise the escaped-response bound: %d bytes", len(encoded))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Query().Get("limit") != "100" {
			t.Errorf("unexpected decision pagination request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ListDecisions(context.Background(), "tsk_0000000000000001", 0, MaxDecisionPage)
	if err != nil || len(got.Decisions) != MaxDecisionPage || got.Decisions[99].Answer.DecisionAnswer.Text != answer.Text {
		t.Fatalf("escaped answered page truncated: records=%d err=%v", len(got.Decisions), err)
	}
}
