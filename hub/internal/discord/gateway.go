package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Dispatch is one Gateway event (op 0), for example MESSAGE_CREATE,
// INTERACTION_CREATE, READY or RESUMED.
type Dispatch struct {
	Type string
	Data json.RawMessage
}

// Gateway keeps one bot connection open: it identifies, heartbeats,
// resumes after drops and re-identifies when a session cannot resume. Events
// reach Handle in order; Handle must return quickly.
type Gateway struct {
	Token   string
	Intents int
	URL     func(ctx context.Context) (string, error) // usually Client.GatewayURL
	Handle  func(Dispatch)
	Log     func(string, ...any)

	sessionID string
	resumeURL string
	seq       int64
}

// ErrFatalClose is a close code that retrying cannot fix (bad token or
// intents the application may not use).
var ErrFatalClose = errors.New("discord gateway: fatal close")

type frame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

func (g *Gateway) logf(format string, args ...any) {
	if g.Log != nil {
		g.Log(format, args...)
	}
}

// Run connects until ctx ends or a fatal close; drops back off and retry.
func (g *Gateway) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		started := time.Now()
		err := g.connect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrFatalClose) {
			return err
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		g.logf("discord gateway: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff + time.Duration(rand.Int64N(int64(time.Second)))):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func withQuery(url string) string {
	if strings.Contains(url, "?") {
		return url
	}
	return strings.TrimRight(url, "/") + "/?v=10&encoding=json"
}

func (g *Gateway) connect(ctx context.Context) error {
	url := g.resumeURL
	if g.sessionID == "" || url == "" {
		var err error
		if url, err = g.URL(ctx); err != nil {
			return fmt.Errorf("gateway url: %w", err)
		}
	}
	conn, _, err := websocket.Dial(ctx, withQuery(url), nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(8 << 20)
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var writeMu sync.Mutex
	send := func(op int, d any) error {
		raw, err := json.Marshal(d)
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(frame{Op: op, D: raw})
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, payload)
	}
	read := func() (frame, error) {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if code := websocket.CloseStatus(err); fatalCode(code) {
				return frame{}, fmt.Errorf("%w %d: %v", ErrFatalClose, code, err)
			} else if code == 4007 || code == 4009 {
				g.sessionID = "" // invalid seq or session timed out: identify afresh
			}
			return frame{}, err
		}
		var f frame
		return f, json.Unmarshal(data, &f)
	}

	hello, err := read()
	if err != nil {
		return err
	}
	if hello.Op != 10 {
		return fmt.Errorf("expected hello, got op %d", hello.Op)
	}
	var h struct {
		HeartbeatInterval int64 `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &h); err != nil || h.HeartbeatInterval <= 0 {
		return fmt.Errorf("bad hello: %s", hello.D)
	}
	if g.sessionID != "" {
		err = send(6, map[string]any{"token": g.Token, "session_id": g.sessionID, "seq": g.seq})
	} else {
		err = send(2, map[string]any{
			"token":      g.Token,
			"intents":    g.Intents,
			"properties": map[string]string{"os": "linux", "browser": "tailterm-discord", "device": "tailterm-discord"},
		})
	}
	if err != nil {
		return err
	}

	var mu sync.Mutex
	acked := true
	heartbeat := func() error {
		mu.Lock()
		seq := g.seq
		acked = false
		mu.Unlock()
		if seq == 0 {
			return send(1, nil)
		}
		return send(1, seq)
	}
	heartbeatErr := make(chan error, 1)
	go func() {
		interval := time.Duration(h.HeartbeatInterval) * time.Millisecond
		timer := time.NewTimer(time.Duration(rand.Float64() * float64(interval)))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			mu.Lock()
			missed := !acked
			mu.Unlock()
			if missed {
				heartbeatErr <- errors.New("heartbeat not acknowledged")
				conn.Close(4000, "zombied connection")
				return
			}
			if err := heartbeat(); err != nil {
				heartbeatErr <- err
				return
			}
			timer.Reset(interval)
		}
	}()

	for {
		f, err := read()
		if err != nil {
			select {
			case hbErr := <-heartbeatErr:
				return hbErr
			default:
				return err
			}
		}
		switch f.Op {
		case 0:
			mu.Lock()
			if f.S != nil {
				g.seq = *f.S
			}
			mu.Unlock()
			if f.T == "READY" {
				var ready struct {
					SessionID        string `json:"session_id"`
					ResumeGatewayURL string `json:"resume_gateway_url"`
				}
				_ = json.Unmarshal(f.D, &ready)
				g.sessionID, g.resumeURL = ready.SessionID, ready.ResumeGatewayURL
			}
			if g.Handle != nil {
				g.Handle(Dispatch{Type: f.T, Data: f.D})
			}
		case 1:
			if err := heartbeat(); err != nil {
				return err
			}
		case 7:
			conn.Close(4000, "reconnect requested")
			return errors.New("server requested reconnect")
		case 9:
			var resumable bool
			_ = json.Unmarshal(f.D, &resumable)
			if !resumable {
				g.sessionID, g.resumeURL, g.seq = "", "", 0
			}
			conn.Close(4000, "invalid session")
			return errors.New("invalid session")
		case 11:
			mu.Lock()
			acked = true
			mu.Unlock()
		}
	}
}

// fatalCode: authentication failed, invalid shard, sharding required,
// invalid API version, invalid or disallowed intents.
func fatalCode(code websocket.StatusCode) bool {
	switch code {
	case 4004, 4010, 4011, 4012, 4013, 4014:
		return true
	}
	return false
}
