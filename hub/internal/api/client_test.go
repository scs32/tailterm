package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newResponseCapClient(t *testing.T, status int, body []byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Token = "token"
	return c
}

func TestDefaultResponseCapIs32MiB(t *testing.T) {
	if defaultMaxResponseBytes != 32<<20 {
		t.Fatalf("default response cap is %d bytes, want %d", defaultMaxResponseBytes, 32<<20)
	}
}

// The release list outgrew the old 4 MiB default cap: tt deployment list read a
// truncated body and failed with "unexpected end of JSON input".
func TestReleasesDecodesListOverFourMiB(t *testing.T) {
	pad := strings.Repeat("x", 1<<20)
	jobs := make([]ReleaseJob, 5)
	for i := range jobs {
		jobs[i] = ReleaseJob{ID: fmt.Sprintf("rel_%d", i), Repository: pad}
	}
	body, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 4<<20 {
		t.Fatalf("fixture is %d bytes, want more than 4 MiB", len(body))
	}
	c := newResponseCapClient(t, http.StatusOK, body)
	got, err := c.Releases(context.Background(), "tsk_test")
	if err != nil {
		t.Fatalf("list over 4 MiB: %v", err)
	}
	if len(got) != len(jobs) || got[4].ID != "rel_4" || got[4].Repository != pad {
		t.Fatalf("decoded %d jobs, want %d intact", len(got), len(jobs))
	}
}

// A small explicit cap exercises the over-cap path without a 32 MiB fixture.
func TestResponseOverCapFailsClearly(t *testing.T) {
	const limit = 64
	fits := []byte(`{"id":"` + strings.Repeat("a", limit-9) + `"}`)
	if len(fits) != limit {
		t.Fatalf("fixture is %d bytes, want %d", len(fits), limit)
	}
	var out Agent
	c := newResponseCapClient(t, http.StatusOK, fits)
	if err := c.doLimited(context.Background(), "GET", "/v1/x", nil, &out, limit); err != nil {
		t.Fatalf("body of exactly the cap: %v", err)
	}
	if len(out.ID) != limit-9 {
		t.Fatalf("decoded id of %d bytes, want %d", len(out.ID), limit-9)
	}

	over := []byte(`{"id":"` + strings.Repeat("a", limit-8) + `"}`)
	c = newResponseCapClient(t, http.StatusOK, over)
	out = Agent{}
	err := c.doLimited(context.Background(), "GET", "/v1/x", nil, &out, limit)
	if err == nil || err.Error() != "hub response exceeds 64 bytes" {
		t.Fatalf("body one byte over the cap: got %v, want hub response exceeds 64 bytes", err)
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		t.Fatalf("over-cap body was parsed: %v", err)
	}
	if out.ID != "" {
		t.Fatalf("over-cap body decoded into out: %q", out.ID)
	}
}

func TestErrorStatusOverCapStaysHTTPError(t *testing.T) {
	const limit = 64
	body := []byte(strings.Repeat("e", limit+10))
	c := newResponseCapClient(t, http.StatusBadGateway, body)
	err := c.doLimited(context.Background(), "GET", "/v1/x", nil, nil, limit)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("got %v, want HTTPError", err)
	}
	if httpErr.Status != http.StatusBadGateway || httpErr.Msg != strings.Repeat("e", limit) {
		t.Fatalf("got status %d and %d message bytes, want 502 and %d", httpErr.Status, len(httpErr.Msg), limit)
	}
}
