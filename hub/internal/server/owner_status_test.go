package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The owner status call over HTTP (wi_475a3df9b345e13e a1). Isolated store;
// the window is fixed, so no live clock decides the figures.
func TestOwnerStatusHTTPRoundTrip(t *testing.T) {
	f := newClient(t)
	ctx := context.Background()
	task, err := f.st.CreateTask(ctx, api.CreateTaskRequest{Name: "Owner status HTTP synthetic"}, f.who)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"bug", "bug", "feature"} {
		if _, err := f.st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: kind, Title: "Synthetic " + kind, Priority: "normal", RequestID: api.NewID("req")}, f.who); err != nil {
			t.Fatal(err)
		}
	}
	c, err := api.NewClient(f.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// To is not after the hub clock, so its latest 21:00 is always past.
	q := api.OwnerStatusQuery{From: now.Add(-time.Hour), To: now, Zone: "America/Los_Angeles"}
	out, err := c.OwnerStatus(ctx, task.ID, q)
	if err != nil {
		t.Fatal(err)
	}
	if out.TaskID != task.ID || out.Zone != q.Zone || !out.From.Equal(q.From) || !out.To.Equal(q.To) ||
		out.Items.Opened != (api.OwnerStatusCounts{Bugs: 2, Features: 1}) || out.Items.OpenNow != (api.OwnerStatusCounts{Bugs: 2, Features: 1}) ||
		out.Baseline == nil || out.Releases.ReleasedSinceBaseline == nil || out.Teams.Stalled == nil {
		t.Fatalf("owner status %+v", out)
	}
	again, err := c.OwnerStatus(ctx, task.ID, q)
	if err != nil || again.Baseline == nil || *again.Baseline != *out.Baseline {
		t.Fatalf("second read %+v %v", again.Baseline, err)
	}

	status := func(query url.Values, deny bool, id string) int {
		t.Helper()
		req, err := http.NewRequest("GET", f.srv.URL+"/v1/tasks/"+id+"/owner-status?"+query.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if deny {
			req.Header.Set("X-Test-Deny", "1")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	good := q.Query()
	with := func(key, value string) url.Values {
		v := url.Values{}
		for k, vs := range good {
			v[k] = append([]string(nil), vs...)
		}
		if value == "" {
			v.Del(key)
		} else {
			v.Set(key, value)
		}
		return v
	}
	for name, v := range map[string]url.Values{
		"missing from":  with("from", ""),
		"missing to":    with("to", ""),
		"unparsable to": with("to", "yesterday"),
		"missing tz":    with("tz", ""),
		"Local tz":      with("tz", "Local"),
		"from after to": with("from", now.Add(time.Hour).Format(time.RFC3339)),
		"over 31 days":  with("from", now.Add(-32*24*time.Hour).Format(time.RFC3339)),
		"unknown tz":    with("tz", "Nowhere/Here"),
	} {
		if code := status(v, false, task.ID); code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", name, code)
		}
	}
	if code := status(good, false, "tsk_0000000000000000"); code != http.StatusNotFound {
		t.Fatalf("unknown project: %d", code)
	}
	if code := status(good, true, task.ID); code != http.StatusForbidden {
		t.Fatalf("denied caller: %d", code)
	}
	var httpErr *api.HTTPError
	if _, err := c.OwnerStatus(ctx, task.ID, api.OwnerStatusQuery{From: now, To: now, Zone: "UTC"}); !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest {
		t.Fatalf("client equal bounds: %v", err)
	}
}
