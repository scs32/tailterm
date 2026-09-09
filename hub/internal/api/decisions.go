package api

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Each record includes both full messages and their structured metadata. Keep
// pages bounded even when JSON escaping expands their text. ListDecisions uses
// a larger bounded reader for this projection; other API reads retain 4 MiB.
const MaxDecisionPage = 100

type DecisionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type DecisionRequest struct {
	Question             string           `json:"question"`
	Options              []DecisionOption `json:"options"`
	RecommendedOptionID  string           `json:"recommendedOptionId"`
	RecommendationReason string           `json:"recommendationReason"`
}

type CreateDecisionRequest struct {
	DecisionRequest
	AgentID          string            `json:"agentId"`
	RequestID        string            `json:"requestId"`
	WorkItems        []MessageWorkItem `json:"workItems,omitempty"`
	WorkOrderMessage *MessageReference `json:"workOrderMessage,omitempty"`
}

type AnswerDecisionRequest struct {
	RequestID string `json:"requestId"`
	OptionID  string `json:"optionId,omitempty"`
	Text      string `json:"text,omitempty"`
	AgentID   string `json:"agentId,omitempty"`
}

type DecisionAnswer struct {
	RequestSeq int64  `json:"requestSeq"`
	OptionID   string `json:"optionId,omitempty"`
	Text       string `json:"text,omitempty"`
}

// DecisionRecord projects resolution separately from immutable board messages.
type DecisionRecord struct {
	Request Message  `json:"request"`
	Answer  *Message `json:"answer,omitempty"`
}

type DecisionList struct {
	Decisions []DecisionRecord `json:"decisions"`
	NextAfter int64            `json:"nextAfter,omitempty"`
}

var decisionOptionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func decisionText(value string, max int, required bool) bool {
	return utf8.ValidString(value) && ValidText(value, max) && (!required || strings.TrimSpace(value) != "")
}

// ValidateDecisionRequest is shared by worker CLI and server; lengths are bytes,
// matching existing hub message limits. It does not validate store references.
func ValidateDecisionRequest(req DecisionRequest) error {
	if !decisionText(req.Question, 2000, true) || len(req.Options) < 2 || len(req.Options) > 5 {
		return fmt.Errorf("%w: provide a question and two to five choices", ErrInvalid)
	}
	seen, recommended := map[string]bool{}, false
	for _, option := range req.Options {
		if !decisionOptionID.MatchString(option.ID) || seen[option.ID] || !decisionText(option.Label, 120, true) || !decisionText(option.Description, 1000, true) {
			return fmt.Errorf("%w: choices need unique IDs, labels and explanations within their limits", ErrInvalid)
		}
		seen[option.ID] = true
		recommended = recommended || option.ID == req.RecommendedOptionID
	}
	if !recommended || !decisionText(req.RecommendationReason, 1000, true) {
		return fmt.Errorf("%w: recommend one listed choice and explain why", ErrInvalid)
	}
	if !ValidText(FormatDecisionRequest(req), MaxTextLen) {
		return fmt.Errorf("%w: rendered decision exceeds the message limit", ErrInvalid)
	}
	return nil
}

func FormatDecisionRequest(req DecisionRequest) string {
	var out strings.Builder
	out.WriteString("Decision requested: " + req.Question)
	for _, option := range req.Options {
		out.WriteString("\n\n" + option.ID + ": " + option.Label)
		if option.ID == req.RecommendedOptionID {
			out.WriteString(" (Recommended)")
		}
		out.WriteString("\n" + option.Description)
	}
	out.WriteString("\n\nRecommendation: " + req.RecommendationReason)
	out.WriteString("\n\nAnswer this decision on the Board. A recommendation is not a submitted answer.")
	return out.String()
}

func ValidateDecisionAnswer(req AnswerDecisionRequest, question DecisionRequest) error {
	if !decisionText(req.Text, 4000, req.OptionID == "") {
		return fmt.Errorf("%w: choose an option or provide a nonblank custom answer (up to 4000 bytes)", ErrInvalid)
	}
	if req.OptionID != "" {
		for _, option := range question.Options {
			if option.ID == req.OptionID {
				return nil
			}
		}
		return fmt.Errorf("%w: choice does not belong to this decision", ErrInvalid)
	}
	return nil
}

func FormatDecisionAnswer(seq int64, req AnswerDecisionRequest, question DecisionRequest) string {
	text := fmt.Sprintf("Decision answer to #%d: ", seq)
	if req.OptionID == "" {
		return text + req.Text
	}
	for _, option := range question.Options {
		if option.ID == req.OptionID {
			text += option.Label + " (" + option.ID + ")"
			break
		}
	}
	if req.Text != "" {
		text += "\n" + req.Text
	}
	return text
}

func (c *Client) CreateDecision(ctx context.Context, taskID string, req CreateDecisionRequest) (Message, error) {
	var out Message
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(taskID)+"/decisions", req, &out)
	return out, err
}

func (c *Client) ListDecisions(ctx context.Context, taskID string, after int64, limit int) (DecisionList, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(limit)}}
	var out DecisionList
	err := c.doLimited(ctx, "GET", "/v1/tasks/"+url.PathEscape(taskID)+"/decisions?"+q.Encode(), nil, &out, 16<<20)
	return out, err
}

func (c *Client) AnswerDecision(ctx context.Context, taskID string, seq int64, req AnswerDecisionRequest) (Message, error) {
	var out Message
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(taskID)+"/decisions/"+strconv.FormatInt(seq, 10)+"/answer", req, &out)
	return out, err
}
