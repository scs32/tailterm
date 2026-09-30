package discord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	key := c.blockKey(routeOf("POST", "/channels/10/messages"))
	c.mu.Lock()
	c.blocked[key] = time.Now().Add(time.Hour)
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

func TestRoutes(t *testing.T) {
	for path, want := range map[string]route{
		"/channels/1/messages":                        {Template: "POST channels/{id}/messages", Major: "1"},
		"/channels/1/messages/99":                     {Template: "POST channels/{id}/messages/{id}", Major: "1"},
		"/guilds/5/channels":                          {Template: "POST guilds/{id}/channels", Major: "5"},
		"/interactions/77/secret-token/callback":      {Template: "POST interactions/{id}/{token}/callback", Major: tokenKey("secret-token"), Exempt: true},
		"/webhooks/7/secret-token/messages/@original": {Template: "POST webhooks/{id}/{token}/messages/@original", Major: tokenKey("secret-token"), Exempt: true},
	} {
		if got := routeOf("POST", path); got != want {
			t.Errorf("routeOf(%s) = %+v, want %+v", path, got, want)
		}
	}
	if got := redactPath("/webhooks/7/secret-token/messages/@original"); strings.Contains(got, "secret") {
		t.Errorf("redactPath kept the token: %s", got)
	}
}

// C12: routes Discord reports as one bucket wait on each other.
func TestSharedBucketsAreRespected(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Bucket", "shared-abc")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset-After", "30")
		_, _ = w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()
	c := &Client{Token: "t", Base: srv.URL, HTTP: srv.Client(), MaxWait: 50 * time.Millisecond}
	if _, err := c.CreateMessage(context.Background(), "10", MessageSend{}); err != nil {
		t.Fatal(err)
	}
	// Another route in the same bucket and channel must now wait for the reset.
	if _, err := c.EditMessage(context.Background(), "10", "5", MessageEdit{}); err != nil {
		// The edit route has not been seen yet, so it only learns the bucket from its own reply.
		t.Fatal(err)
	}
	if _, err := c.CreateMessage(context.Background(), "10", MessageSend{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a send in an exhausted shared bucket = %v, want ErrRateLimited", err)
	}
	if _, err := c.EditMessage(context.Background(), "10", "6", MessageEdit{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("an edit in the exhausted shared bucket = %v, want ErrRateLimited", err)
	}
	// A different channel is a different major parameter.
	if _, err := c.CreateMessage(context.Background(), "11", MessageSend{}); err != nil {
		t.Fatalf("another channel was blocked: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("%d requests reached Discord, want 3", calls.Load())
	}
}

// C11 and C10: interaction replies ignore the global limit, and transport
// errors never carry their tokens.
func TestInteractionRepliesAreExemptAndRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	c := &Client{Token: "t", Base: srv.URL, HTTP: srv.Client(), MaxWait: 50 * time.Millisecond}
	c.init()
	c.mu.Lock()
	c.globalUntil = time.Now().Add(time.Hour)
	c.mu.Unlock()
	if err := c.RespondInteraction(context.Background(), "77", "secret-token", InteractionResponse{Type: ResponseDeferredMessage}); err != nil {
		t.Fatalf("an interaction reply waited on the global limit: %v", err)
	}
	if _, err := c.CreateMessage(context.Background(), "10", MessageSend{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("an ordinary send ignored the global limit: %v", err)
	}
	srv.Close() // now every request fails in transport
	err := c.EditInteractionResponse(context.Background(), "7", "secret-token", MessageEdit{})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("transport error = %v; it must not contain the token", err)
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

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// Production incident 2026-09-24: Discord reports each interaction's single
// reply as an exhausted bucket; that must not delay the next interaction.
func TestOneInteractionsLimitDoesNotDelayTheNext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Bucket", "interaction-callback")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset-After", "30")
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c := &Client{Token: "t", Base: srv.URL, HTTP: srv.Client(), MaxWait: 50 * time.Millisecond}
	for i, token := range []string{"first-token", "second-token", "third-token"} {
		if err := c.RespondInteraction(context.Background(), strconv.Itoa(100+i), token, InteractionResponse{Type: ResponseDeferredMessage}); err != nil {
			t.Fatalf("interaction %d waited on another interaction's limit: %v", i+1, err)
		}
		if err := c.EditInteractionResponse(context.Background(), "7", token, MessageEdit{}); err != nil {
			t.Fatalf("edit %d waited on another interaction's limit: %v", i+1, err)
		}
	}
	for k := range c.blocked {
		if strings.Contains(k, "token") {
			t.Fatalf("a raw token became a map key: %s", k)
		}
	}
}

// Owner helper conversation (docs/discord-helper-chat.md): the three REST
// calls send Discord's documented shapes.
func TestHelperRESTCalls(t *testing.T) {
	type call struct {
		Method, Path string
		Body         json.RawMessage
	}
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path, body})
		mu.Unlock()
		switch {
		case r.URL.Path == "/users/@me/channels":
			_, _ = w.Write([]byte(`{"id":"900","type":1}`))
		case strings.HasSuffix(r.URL.Path, "/threads") && strings.Contains(r.URL.Path, "/messages/77/"):
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"code":160004,"message":"A thread has already been created for this message"}`))
		case strings.HasSuffix(r.URL.Path, "/threads"):
			_, _ = w.Write([]byte(`{"id":"55","type":11,"parent_id":"10"}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	c := &Client{Token: "t", Base: srv.URL, HTTP: srv.Client()}
	ctx := context.Background()
	dm, err := c.CreateDM(ctx, "42")
	if err != nil || dm.ID != "900" || dm.Type != ChannelDM {
		t.Fatalf("CreateDM = %+v %v", dm, err)
	}
	th, err := c.StartThreadFromMessage(ctx, "10", "55", StartThread{Name: "Owner helper", AutoArchiveDuration: 1440})
	if err != nil || th.ID != "55" || th.Type != ChannelPublicThread {
		t.Fatalf("StartThreadFromMessage = %+v %v", th, err)
	}
	if _, err := c.StartThreadFromMessage(ctx, "10", "77", StartThread{Name: "x"}); !IsCode(err, CodeThreadAlreadyCreated) {
		t.Fatalf("a second thread start should be code 160004, got %v", err)
	}
	global := []Command{{Name: "status", Description: "d", Contexts: []int{ContextBotDM}, IntegrationTypes: []int{IntegrationGuildInstall}}}
	if err := c.SetGlobalCommands(ctx, "app", global); err != nil {
		t.Fatal(err)
	}
	if err := c.SetGlobalCommands(ctx, "app", nil); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"POST", "/users/@me/channels", json.RawMessage(`{"recipient_id":"42"}`)},
		{"POST", "/channels/10/messages/55/threads", json.RawMessage(`{"name":"Owner helper","auto_archive_duration":1440}`)},
		{"POST", "/channels/10/messages/77/threads", json.RawMessage(`{"name":"x"}`)},
		{"PUT", "/applications/app/commands", json.RawMessage(`[{"name":"status","description":"d","contexts":[1],"integration_types":[0]}]`)},
		{"PUT", "/applications/app/commands", json.RawMessage(`[]`)},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i, w := range want {
		if calls[i].Method != w.Method || calls[i].Path != w.Path || string(calls[i].Body) != string(w.Body) {
			t.Errorf("call %d = %s %s %s, want %s %s %s", i, calls[i].Method, calls[i].Path, calls[i].Body, w.Method, w.Path, w.Body)
		}
	}
	// A guild command keeps its old JSON: no contexts or integration types.
	raw, _ := json.Marshal(Command{Name: "status", Description: "d"})
	if string(raw) != `{"name":"status","description":"d"}` {
		t.Errorf("guild command JSON changed: %s", raw)
	}
	for path, want := range map[string]string{
		"/users/@me/channels":              "POST users/@me/channels",
		"/channels/10/messages/55/threads": "POST channels/{id}/messages/{id}/threads",
		"/applications/1/commands":         "POST applications/{id}/commands",
	} {
		if got := routeOf("POST", path).Template; got != want {
			t.Errorf("routeOf(%s) = %s, want %s", path, got, want)
		}
	}
}
