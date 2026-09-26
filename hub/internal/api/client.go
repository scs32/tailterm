package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a small JSON client for the hub.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

func NewClient(base string, timeout time.Duration) (*Client, error) {
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("hub URL must be http(s)://host[:port], got %q", base)
	}
	return &Client{Base: base, HTTP: &http.Client{Timeout: timeout}}, nil
}

type HTTPError struct {
	Status   int
	Msg      string
	Code     string
	Problems []Problem
}

func (e *HTTPError) Error() string { return fmt.Sprintf("hub: %d %s", e.Status, e.Msg) }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doLimited(ctx, method, path, body, out, 4<<20)
}

func (c *Client) doLimited(ctx context.Context, method, path string, body, out any, maxResponse int64) error {
	return c.doLimitedJSON(ctx, method, path, body, out, maxResponse, true)
}

func (c *Client) doLimitedJSON(ctx context.Context, method, path string, body, out any, maxResponse int64, escapeHTML bool) error {
	var buf io.Reader
	if body != nil {
		var data []byte
		if escapeHTML {
			var err error
			data, err = json.Marshal(body)
			if err != nil {
				return err
			}
		} else {
			var encoded bytes.Buffer
			encoder := json.NewEncoder(&encoded)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(body); err != nil {
				return err
			}
			data = bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
		}
		buf = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, buf)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		if res.StatusCode == http.StatusConflict {
			var gap WorkItemHistoryGapResponse
			if json.Unmarshal(data, &gap) == nil && gap.Gap.ReasonCode != "" {
				return &WorkItemHistoryGapError{Response: gap}
			}
		}
		var e ErrorResponse
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &HTTPError{Status: res.StatusCode, Msg: e.Error, Code: e.Code, Problems: e.Problems}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) Whoami(ctx context.Context) (Caller, error) {
	var out Caller
	return out, c.do(ctx, "GET", "/v1/whoami", nil, &out)
}

func (c *Client) CreateTask(ctx context.Context, req CreateTaskRequest) (Task, error) {
	var out Task
	return out, c.do(ctx, "POST", "/v1/tasks", req, &out)
}

func (c *Client) ListTasks(ctx context.Context) ([]Task, error) {
	var out TaskList
	return out.Tasks, c.do(ctx, "GET", "/v1/tasks", nil, &out)
}

func (c *Client) GetTask(ctx context.Context, id string) (TaskDetail, error) {
	var out TaskDetail
	return out, c.do(ctx, "GET", "/v1/tasks/"+id, nil, &out)
}

func (c *Client) UpdateTask(ctx context.Context, id string, req UpdateTaskRequest) (Task, error) {
	var out Task
	return out, c.do(ctx, "PATCH", "/v1/tasks/"+id, req, &out)
}

func (c *Client) GetTeamCloseReceipt(ctx context.Context, task, requestID string) (TeamCloseResult, error) {
	var out TeamCloseResult
	return out, c.do(ctx, "GET", "/v1/tasks/"+task+"/team-close/receipts/"+url.PathEscape(requestID), nil, &out)
}

func (c *Client) CloseTask(ctx context.Context, id string) (Task, error) {
	var out Task
	return out, c.do(ctx, "DELETE", "/v1/tasks/"+id, nil, &out)
}

func (c *Client) PauseProject(ctx context.Context, id string, req PauseProjectRequest) (ProjectPauseStatus, error) {
	var out ProjectPauseStatus
	return out, c.do(ctx, "POST", "/v1/tasks/"+id+"/pause", req, &out)
}

func (c *Client) GetProjectPause(ctx context.Context, id string) (ProjectPauseStatus, error) {
	var out ProjectPauseStatus
	return out, c.do(ctx, "GET", "/v1/tasks/"+id+"/pause", nil, &out)
}

func (c *Client) ResumeProject(ctx context.Context, id string, req ResumeProjectRequest) (ProjectPauseStatus, error) {
	var out ProjectPauseStatus
	return out, c.do(ctx, "POST", "/v1/tasks/"+id+"/resume", req, &out)
}

func (c *Client) ConfirmProjectResume(ctx context.Context, id string, req ConfirmProjectResumeRequest) (ProjectPauseStatus, error) {
	var out ProjectPauseStatus
	return out, c.do(ctx, "POST", "/v1/tasks/"+id+"/resume/confirm", req, &out)
}

func (c *Client) ResolvePauseHandoff(ctx context.Context, id string, req ResolvePauseHandoffRequest) (ProjectPauseStatus, error) {
	var out ProjectPauseStatus
	return out, c.do(ctx, "POST", "/v1/tasks/"+id+"/pause/handoff", req, &out)
}

func (c *Client) AddAgent(ctx context.Context, task string, req AddAgentRequest) (Agent, error) {
	var out Agent
	// Preserve the prepared context's byte bound instead of expanding raw JSON
	// with HTML-safe escapes inside the registration envelope.
	return out, c.doLimitedJSON(ctx, "POST", "/v1/tasks/"+task+"/agents", req, &out, 4<<20, false)
}

func (c *Client) CreateAllocationIntent(ctx context.Context, task string, req CreateAllocationIntentRequest) (AllocationIntent, error) {
	var out AllocationIntent
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/allocation-intents", req, &out)
}

func (c *Client) GetAllocationIntent(ctx context.Context, task, agent string) (AllocationIntent, error) {
	var out AllocationIntent
	return out, c.do(ctx, "GET", "/v1/tasks/"+task+"/allocation-intents/"+agent, nil, &out)
}

func (c *Client) GetAgent(ctx context.Context, task, agent string) (Agent, error) {
	var out Agent
	return out, c.do(ctx, "GET", "/v1/tasks/"+task+"/agents/"+agent, nil, &out)
}

func (c *Client) GetAgentWorkItemContext(ctx context.Context, task, agent, runID string) (AgentWorkItemContext, error) {
	var out AgentWorkItemContext
	q := url.Values{}
	q.Set("runId", runID)
	err := c.do(ctx, "GET", "/v1/tasks/"+task+"/agents/"+agent+"/work-context?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) ListAgents(ctx context.Context, task string) ([]Agent, error) {
	var out AgentList
	return out.Agents, c.do(ctx, "GET", "/v1/tasks/"+task+"/agents", nil, &out)
}

func (c *Client) UpdateAgent(ctx context.Context, task, agent string, req UpdateAgentRequest) (Agent, error) {
	var out Agent
	return out, c.do(ctx, "PATCH", "/v1/tasks/"+task+"/agents/"+agent, req, &out)
}

func (c *Client) CloseAgent(ctx context.Context, task, agent, runID string) (Agent, error) {
	var out Agent
	q := url.Values{}
	q.Set("runId", runID)
	return out, c.do(ctx, "DELETE", "/v1/tasks/"+task+"/agents/"+agent+"?"+q.Encode(), nil, &out)
}

func (c *Client) CloseItemTeam(ctx context.Context, task string, req TeamCloseRequest) (TeamCloseResult, error) {
	var out TeamCloseResult
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/team-close", req, &out)
}

func (c *Client) PostMessage(ctx context.Context, task string, req PostMessageRequest) (Message, error) {
	var out Message
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/messages", req, &out)
}

// ListObligations lists a task's obligations. Passing the caller's own agent
// and current run marks that run's queued obligations delivered.
func (c *Client) ListObligations(ctx context.Context, task, agent, run string, open, overdue bool) ([]Obligation, error) {
	q := url.Values{}
	if agent != "" {
		q.Set("agentId", agent)
	}
	if run != "" {
		q.Set("runId", run)
	}
	if open {
		q.Set("open", "1")
	}
	if overdue {
		q.Set("overdue", "1")
	}
	var out ObligationList
	return out.Obligations, c.do(ctx, "GET", "/v1/tasks/"+task+"/obligations?"+q.Encode(), nil, &out)
}

// ListRecentWithdrawn returns at most the five newest closed withdrawals.
func (c *Client) ListRecentWithdrawn(ctx context.Context, task string) ([]Obligation, error) {
	var out ObligationList
	return out.Obligations, c.do(ctx, "GET", "/v1/tasks/"+task+"/obligations?recentWithdrawn=1", nil, &out)
}

// ListObligationsFrom lists an agent's obligations (any state) for messages
// fromSeq..toSeq, without marking anything delivered.
func (c *Client) ListObligationsFrom(ctx context.Context, task, agent string, fromSeq, toSeq int64) ([]Obligation, error) {
	q := url.Values{}
	q.Set("agentId", agent)
	q.Set("fromSeq", strconv.FormatInt(fromSeq, 10))
	q.Set("toSeq", strconv.FormatInt(toSeq, 10))
	var out ObligationList
	return out.Obligations, c.do(ctx, "GET", "/v1/tasks/"+task+"/obligations?"+q.Encode(), nil, &out)
}

// ObligationAction is "ack" or "progress" on the obligation message seq created.
func (c *Client) ObligationAction(ctx context.Context, task string, seq int64, action string, req ObligationActionRequest) (Obligation, error) {
	var out Obligation
	return out, c.do(ctx, "POST", fmt.Sprintf("/v1/tasks/%s/messages/%d/%s", task, seq, action), req, &out)
}

func (c *Client) WithdrawObligation(ctx context.Context, task, obligation string, req ObligationWithdrawRequest) (Obligation, error) {
	var out Obligation
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/obligations/"+obligation+"/withdraw", req, &out)
}

// LeaseWakeJob returns the next due broker wake for an agent run, or nil.
func (c *Client) LeaseWakeJob(ctx context.Context, task, agent, run string) (*WakeJob, error) {
	var out WakeJob
	if err := c.do(ctx, "POST", "/v1/tasks/"+task+"/agents/"+agent+"/wake-jobs/lease", ObligationActionRequest{AgentID: agent, RunID: run}, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, nil
	}
	return &out, nil
}

func (c *Client) ReportWakeJob(ctx context.Context, task, job string, report WakeJobReport) error {
	return c.do(ctx, "POST", "/v1/tasks/"+task+"/wake-jobs/"+job+"/report", report, nil)
}

func (c *Client) ReassignObligation(ctx context.Context, task, obligation string, req ObligationReassignRequest) (Message, error) {
	var out Message
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/obligations/"+obligation+"/reassign", req, &out)
}

// NudgeObligation queues an immediate wake of an open obligation's recipient.
// A repeated requestID returns the original nudge instead of nudging again.
func (c *Client) NudgeObligation(ctx context.Context, task, obligation, requestID string) (ObligationNudgeResult, error) {
	var out ObligationNudgeResult
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/obligations/"+obligation+"/nudge", ObligationNudgeRequest{RequestID: requestID}, &out)
}

func (c *Client) ListMessageChecks(ctx context.Context, task string, after int64, limit int) ([]MessageCheck, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	q.Set("limit", strconv.Itoa(limit))
	var out MessageCheckList
	return out.Checks, c.do(ctx, "GET", "/v1/tasks/"+task+"/message-checks?"+q.Encode(), nil, &out)
}

func (c *Client) ListMessages(ctx context.Context, task string, after int64, to string, limit int) ([]Message, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	if to != "" {
		q.Set("to", to)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out MessageList
	return out.Messages, c.do(ctx, "GET", "/v1/tasks/"+task+"/messages?"+q.Encode(), nil, &out)
}

func (c *Client) ownerAction(ctx context.Context, path string, req any) (OwnerActionResult, error) {
	var out OwnerActionResult
	return out, c.do(ctx, "POST", path, req, &out)
}

// ExtendObligation, AnswerObligation and CancelObligation are the owner's
// phase-3 controls; ResumeRetiredAgent re-enables a retired agent's wake-ups.
func (c *Client) ExtendObligation(ctx context.Context, task, obligation string, req ObligationExtendRequest) (OwnerActionResult, error) {
	return c.ownerAction(ctx, "/v1/tasks/"+task+"/obligations/"+obligation+"/extend", req)
}

func (c *Client) AnswerObligation(ctx context.Context, task, obligation string, req ObligationAnswerRequest) (OwnerActionResult, error) {
	return c.ownerAction(ctx, "/v1/tasks/"+task+"/obligations/"+obligation+"/answer", req)
}

func (c *Client) CancelObligation(ctx context.Context, task, obligation string, req ObligationCancelRequest) (OwnerActionResult, error) {
	return c.ownerAction(ctx, "/v1/tasks/"+task+"/obligations/"+obligation+"/cancel", req)
}

func (c *Client) ResumeRetiredAgent(ctx context.Context, task, agent string, req AgentResumeRequest) (OwnerActionResult, error) {
	return c.ownerAction(ctx, "/v1/tasks/"+task+"/agents/"+agent+"/resume", req)
}

// LatestMessages returns a task's newest messages, newest first.
func (c *Client) LatestMessages(ctx context.Context, task string, limit int) ([]Message, error) {
	q := url.Values{}
	q.Set("latest", "1")
	q.Set("limit", strconv.Itoa(limit))
	var out MessageList
	return out.Messages, c.do(ctx, "GET", "/v1/tasks/"+task+"/messages?"+q.Encode(), nil, &out)
}

func (c *Client) MarkRead(ctx context.Context, task string, req MarkReadRequest) error {
	return c.do(ctx, "POST", "/v1/tasks/"+task+"/messages/read", req, nil)
}

func (c *Client) PostEvent(ctx context.Context, task string, req PostEventRequest) (Event, error) {
	var out Event
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/events", req, &out)
}

// Events long-polls a task feed (or the global feed when task is empty).
func (c *Client) Events(ctx context.Context, task string, after int64, wait time.Duration, limit int) (EventList, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	if wait > 0 {
		q.Set("wait", wait.String())
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/events?"
	if task != "" {
		path = "/v1/tasks/" + task + "/events?"
	}
	var out EventList
	return out, c.do(ctx, "GET", path+q.Encode(), nil, &out)
}

func (c *Client) ReportCleanup(ctx context.Context, task, agent string, req CleanupRequest) (Agent, error) {
	var out Agent
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/agents/"+agent+"/cleanup", req, &out)
}

func (c *Client) ListReviewConvergence(ctx context.Context, task string) ([]ReviewConvergence, error) {
	var out []ReviewConvergence
	err := c.do(ctx, "GET", "/v1/tasks/"+task+"/review-convergence", nil, &out)
	return out, err
}
