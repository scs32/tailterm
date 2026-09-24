package discord

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRESTWaitsOutRateLimits(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot secret" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"retry_after":0.05,"global":false}`))
		case 2:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset-After", "0.05")
			_, _ = w.Write([]byte(`{"id":"1","content":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"2","content":"ok"}`))
		}
	}))
	defer srv.Close()
	c := &Client{Token: "secret", Base: srv.URL, HTTP: srv.Client()}
	start := time.Now()
	m, err := c.CreateMessage(context.Background(), "10", MessageSend{Content: "hi"})
	if err != nil || m.ID != "1" || calls.Load() != 2 {
		t.Fatalf("after a 429: %+v %v (%d calls)", m, err, calls.Load())
	}
	if _, err := c.CreateMessage(context.Background(), "10", MessageSend{Content: "again"}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 90*time.Millisecond {
		t.Errorf("did not wait for retry_after and the exhausted bucket (%s)", time.Since(start))
	}
	c.MaxWait = 10 * time.Millisecond
	c.mu.Lock()
	c.blocked[bucketKey("POST", "/channels/10/messages")] = time.Now().Add(time.Hour)
	c.mu.Unlock()
	if _, err := c.CreateMessage(context.Background(), "10", MessageSend{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a long block should give up with ErrRateLimited, got %v", err)
	}
}

func TestRESTErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"code":10003,"message":"Unknown Channel"}`))
	}))
	defer srv.Close()
	c := &Client{Token: "t", Base: srv.URL, HTTP: srv.Client()}
	_, err := c.CreateMessage(context.Background(), "10", MessageSend{})
	if !Permanent(err) || !IsCode(err, CodeUnknownChannel) {
		t.Fatalf("err = %v", err)
	}
	if Permanent(&HTTPError{Status: 500}) || Permanent(errors.New("network")) {
		t.Error("server and network errors must be retryable")
	}
}

func TestBucketKeys(t *testing.T) {
	for path, want := range map[string]string{
		"/channels/1/messages":               "POST channels/1/messages",
		"/channels/1/messages/99":            "POST channels/1/messages/{id}",
		"/guilds/5/channels":                 "POST guilds/5/channels",
		"/webhooks/7/tok/messages/@original": "POST webhooks/7/tok/messages/@original",
	} {
		if got := bucketKey("POST", path); got != want {
			t.Errorf("bucketKey(%s) = %s, want %s", path, got, want)
		}
	}
}

// fakeGateway speaks enough of the Gateway protocol to test sessions.
type fakeGateway struct {
	t        *testing.T
	mu       sync.Mutex
	opened   int
	received []frame
	script   func(conn *websocket.Conn, n int) // per connection number
}

func (g *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		g.t.Error(err)
		return
	}
	defer conn.CloseNow()
	g.mu.Lock()
	g.opened++
	n := g.opened
	g.mu.Unlock()
	g.script(conn, n)
}

func (g *fakeGateway) send(conn *websocket.Conn, f frame) {
	raw, _ := json.Marshal(f)
	_ = conn.Write(context.Background(), websocket.MessageText, raw)
}

func (g *fakeGateway) read(conn *websocket.Conn) frame {
	_, raw, err := conn.Read(context.Background())
	if err != nil {
		return frame{Op: -1}
	}
	var f frame
	_ = json.Unmarshal(raw, &f)
	g.mu.Lock()
	g.received = append(g.received, f)
	g.mu.Unlock()
	return f
}

func seq(n int64) *int64 { return &n }

func TestGatewayIdentifiesResumesAndReidentifies(t *testing.T) {
	events := make(chan string, 16)
	g := &fakeGateway{t: t}
	g.script = func(conn *websocket.Conn, n int) {
		g.send(conn, frame{Op: 10, D: json.RawMessage(`{"heartbeat_interval":45000}`)})
		first := g.read(conn)
		switch n {
		case 1:
			if first.Op != 2 {
				t.Errorf("connection 1 opened with op %d, want identify", first.Op)
			}
			var id struct {
				Intents int `json:"intents"`
			}
			_ = json.Unmarshal(first.D, &id)
			if id.Intents != IntentGuilds|IntentGuildMessages|IntentMessageContent {
				t.Errorf("intents = %d", id.Intents)
			}
			g.send(conn, frame{Op: 0, T: "READY", S: seq(1), D: json.RawMessage(`{"session_id":"s1","resume_gateway_url":"` + resumeURL + `"}`)})
			g.send(conn, frame{Op: 0, T: "MESSAGE_CREATE", S: seq(2), D: json.RawMessage(`{"id":"m1"}`)})
			g.send(conn, frame{Op: 7}) // please reconnect
			g.read(conn)
		case 2:
			var resume struct {
				SessionID string `json:"session_id"`
				Seq       int64  `json:"seq"`
			}
			_ = json.Unmarshal(first.D, &resume)
			if first.Op != 6 || resume.SessionID != "s1" || resume.Seq != 2 {
				t.Errorf("connection 2 = op %d %+v, want resume of s1 at 2", first.Op, resume)
			}
			g.send(conn, frame{Op: 0, T: "RESUMED", S: seq(3), D: json.RawMessage(`{}`)})
			g.send(conn, frame{Op: 9, D: json.RawMessage(`false`)}) // session cannot resume
			g.read(conn)
		case 3:
			if first.Op != 2 {
				t.Errorf("after an invalid session, op %d, want a fresh identify", first.Op)
			}
			g.send(conn, frame{Op: 0, T: "READY", S: seq(1), D: json.RawMessage(`{"session_id":"s2","resume_gateway_url":"` + resumeURL + `"}`)})
			g.read(conn)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	defer srv.Close()
	resumeURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw := &Gateway{Token: "t", Intents: IntentGuilds | IntentGuildMessages | IntentMessageContent,
		URL:    func(context.Context) (string, error) { return resumeURL, nil },
		Handle: func(d Dispatch) { events <- d.Type },
		Log:    t.Logf}
	done := make(chan error, 1)
	go func() { done <- gw.Run(ctx) }()
	var got []string
	for len(got) < 4 {
		select {
		case e := <-events:
			got = append(got, e)
		case <-ctx.Done():
			t.Fatalf("events so far: %v", got)
		}
	}
	cancel()
	<-done
	if strings.Join(got, ",") != "READY,MESSAGE_CREATE,RESUMED,READY" {
		t.Fatalf("events = %v", got)
	}
}

var resumeURL string

func TestGatewayFatalClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		raw, _ := json.Marshal(frame{Op: 10, D: json.RawMessage(`{"heartbeat_interval":45000}`)})
		_ = conn.Write(context.Background(), websocket.MessageText, raw)
		_, _, _ = conn.Read(context.Background())
		conn.Close(4014, "Disallowed intent(s)")
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gw := &Gateway{Token: "t", URL: func(context.Context) (string, error) { return url, nil }, Log: t.Logf}
	if err := gw.Run(ctx); !errors.Is(err, ErrFatalClose) {
		t.Fatalf("Run = %v, want ErrFatalClose", err)
	}
}
