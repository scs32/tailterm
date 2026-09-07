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
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("hub: %d %s", e.Status, e.Msg) }

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		buf = bytes.NewReader(b)
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
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		var e ErrorResponse
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &HTTPError{Status: res.StatusCode, Msg: e.Error}
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

func (c *Client) CloseTask(ctx context.Context, id string) (Task, error) {
	var out Task
	return out, c.do(ctx, "DELETE", "/v1/tasks/"+id, nil, &out)
}

func (c *Client) AddAgent(ctx context.Context, task string, req AddAgentRequest) (Agent, error) {
	var out Agent
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/agents", req, &out)
}

func (c *Client) GetAgent(ctx context.Context, task, agent string) (Agent, error) {
	var out Agent
	return out, c.do(ctx, "GET", "/v1/tasks/"+task+"/agents/"+agent, nil, &out)
}

func (c *Client) ListAgents(ctx context.Context, task string) ([]Agent, error) {
	var out AgentList
	return out.Agents, c.do(ctx, "GET", "/v1/tasks/"+task+"/agents", nil, &out)
}

func (c *Client) UpdateAgent(ctx context.Context, task, agent string, req UpdateAgentRequest) (Agent, error) {
	var out Agent
	return out, c.do(ctx, "PATCH", "/v1/tasks/"+task+"/agents/"+agent, req, &out)
}

func (c *Client) CloseAgent(ctx context.Context, task, agent string) (Agent, error) {
	var out Agent
	return out, c.do(ctx, "DELETE", "/v1/tasks/"+task+"/agents/"+agent, nil, &out)
}

func (c *Client) PostMessage(ctx context.Context, task string, req PostMessageRequest) (Message, error) {
	var out Message
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/messages", req, &out)
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
