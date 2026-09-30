package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

// An integrated verification import carries the whole plan and receipt; a
// 72-check receipt is about 106 KB, and on 09-30 the shared 64 KiB limit
// refused it as "invalid scope metadata request" before any validation.
func TestReleaseActionDecodesBodiesOverSharedLimit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &client{t: t, st: st, who: api.Caller{Node: "devbox", User: "stephen@example.com"}}
	c.srv = httptest.NewServer(New(st, func(*http.Request) (api.Caller, error) { return c.who, nil }))
	t.Cleanup(c.srv.Close)
	task := c.task("release-body")
	post := func(body []byte) (int, api.ErrorResponse) {
		resp, err := http.Post(c.srv.URL+"/v1/tasks/"+task.ID+"/releases/actions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out api.ErrorResponse
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}
	sized := func(size int) []byte {
		body, _ := json.Marshal(api.ReleaseRequest{Operation: "verification", JobID: "rel_" + strings.Repeat("0", 16), RequestID: strings.Repeat("r", size)})
		return body
	}
	const invalid = "invalid scope metadata request"
	if _, got := post(sized(96 << 10)); strings.HasPrefix(got.Error, invalid) {
		t.Fatalf("96 KiB import refused at decode: %+v", got)
	}
	for _, tc := range []struct {
		body       []byte
		want, code string
	}{
		{sized(int(api.MaxVerificationBody)), invalid + ": body exceeds 1048576 bytes", "body-too-large"},
		{[]byte(`{"operation":"verification","bogus":1}`), invalid + `: unknown field "bogus"`, "unknown-field"},
		{[]byte(`{"operation":"verification"} {}`), invalid + ": trailing data after the JSON object", "trailing-data"},
	} {
		if status, got := post(tc.body); status != 400 || got.Error != tc.want || got.Code != tc.code {
			t.Fatalf("%.40q: %d %+v, want %q %q", tc.body, status, got, tc.want, tc.code)
		}
	}
}
