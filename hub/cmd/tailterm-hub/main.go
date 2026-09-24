// Command tailterm-hub serves coordination on an existing private network or optional tsnet node.
//
// Environment:
//
// TAILTERM_TCP_LISTEN normal TCP listener; disables embedded Tailscale
// TAILTERM_TOKEN_FILE required credential file for TCP mode
// TAILTERM_MAX_AGENTS active agent limit per task (1..32)
//
//	TS_AUTHKEY        Tailscale auth key (first start only; state persists)
//	TS_HOSTNAME       tailnet hostname (default tailterm-hub)
//	TAILTERM_STATE    state directory for tsnet and hub.sqlite (default /state)
//	TAILTERM_LISTEN   tailnet listen address (default :80)
//	TAILTERM_DEV_LISTEN  plain TCP listen address with a fixed dev identity,
//	                  for local testing without a tailnet (e.g. 127.0.0.1:8080)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tailscale.com/tsnet"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/broker"
	"github.com/scs32/tailterm/hub/internal/jev"
	"github.com/scs32/tailterm/hub/internal/monitor"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func scheduleMonitorConfig() (monitor.Config, error) {
	config := monitor.DefaultConfig()
	for _, setting := range []struct {
		key  string
		into *time.Duration
	}{
		{"TAILTERM_SCHEDULE_MONITOR_INTERVAL", &config.Interval},
		{"TAILTERM_SCHEDULE_MONITOR_STALL_AFTER", &config.StallAfter},
		{"TAILTERM_SCHEDULE_MONITOR_WORKER_SILENCE", &config.WorkerSilence},
		{"TAILTERM_SCHEDULE_MONITOR_INITIAL_BACKOFF", &config.InitialBackoff},
		{"TAILTERM_SCHEDULE_MONITOR_MAX_BACKOFF", &config.MaxBackoff},
		{"TAILTERM_SCHEDULE_MONITOR_NOTICE_RETENTION", &config.NoticeRetention},
	} {
		if raw := os.Getenv(setting.key); raw != "" {
			value, err := time.ParseDuration(raw)
			if err != nil || value <= 0 {
				return monitor.Config{}, fmt.Errorf("%s must be a positive duration: %q", setting.key, raw)
			}
			*setting.into = value
		}
	}
	if config.MaxBackoff < config.InitialBackoff {
		return monitor.Config{}, fmt.Errorf("monitor maximum backoff must be at least initial backoff")
	}
	return config, nil
}

func main() {
	stateDir := env("TAILTERM_STATE", "/state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		log.Fatalf("state dir: %v", err)
	}
	st, err := store.Open(filepath.Join(stateDir, "hub.sqlite"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if n, err := strconv.Atoi(os.Getenv("TAILTERM_MAX_AGENTS")); err == nil && n > 0 && n <= api.MaxAgentsPerTask {
		st.MaxAgents = n
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	monitorConfig, err := scheduleMonitorConfig()
	if err != nil {
		log.Fatal(err)
	}
	enforcer, err := monitor.New(st, monitorConfig)
	if err != nil {
		log.Fatal(err)
	}
	monitorDone := enforcer.Start(ctx, func(outcome monitor.Outcome) {
		if outcome.Log {
			log.Printf("%s", outcome.Summary())
		}
	})

	defer func() { stop(); <-monitorDone }()

	// Broker phase 2a: re-wake, nudge and escalate open obligations.
	obligationBroker := &broker.Broker{Store: st, Interval: 30 * time.Second, Log: log.Printf}
	brokerDone := obligationBroker.Start(ctx)
	defer func() { stop(); <-brokerDone }()

	// Jev scoring is log-only and optional: without a key file, agent posts
	// are recorded as jev disabled and nothing leaves the hub.
	if keyFile := os.Getenv("TAILTERM_TYPESAFE_KEY_FILE"); keyFile != "" {
		key, keyErr := os.ReadFile(keyFile)
		if keyErr != nil || strings.TrimSpace(string(key)) == "" {
			log.Printf("jev scoring disabled: cannot read TAILTERM_TYPESAFE_KEY_FILE: %v", keyErr)
		} else {
			st.EnableJevScoring()
			scorer := &jev.Scorer{
				Client: &jev.Client{
					URL:   env("TAILTERM_TYPESAFE_URL", jev.DefaultURL),
					Key:   strings.TrimSpace(string(key)),
					Model: env("TAILTERM_TYPESAFE_MODEL", jev.DefaultModel),
					HTTP:  &http.Client{},
				},
				Checks: st, Concurrency: 4, Timeout: 3 * time.Second, Interval: 2 * time.Second,
				Log: log.Printf,
			}
			scorerDone := scorer.Start(ctx)
			defer func() { stop(); <-scorerDone }()
			log.Printf("jev scoring enabled (log-only)")
		}
	}

	var ln net.Listener
	var identity server.Identity
	if addr := os.Getenv("TAILTERM_TCP_LISTEN"); addr != "" {
		token, readErr := os.ReadFile(os.Getenv("TAILTERM_TOKEN_FILE"))
		if readErr != nil {
			log.Fatalf("read hub token: %v", readErr)
		}
		identity, err = server.TokenIdentity(strings.TrimSpace(string(token)), api.Caller{Node: "workspace", User: "owner"})
		if err != nil {
			log.Fatal(err)
		}
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("TCP listen: %v", err)
		}
		log.Printf("hub listening on %s with token authentication (no embedded Tailscale)", addr)
	} else if dev := os.Getenv("TAILTERM_DEV_LISTEN"); dev != "" {
		ln, err = net.Listen("tcp", dev)
		if err != nil {
			log.Fatalf("dev listen: %v", err)
		}
		identity = server.StaticIdentity(api.Caller{Node: "dev", User: "dev@local"})
		log.Printf("dev mode: listening on %s with a fixed identity", dev)
	} else {
		ts := &tsnet.Server{
			Dir:      filepath.Join(stateDir, "tsnet"),
			Hostname: env("TS_HOSTNAME", "tailterm-hub"),
			AuthKey:  os.Getenv("TS_AUTHKEY"),
			Logf:     log.Printf,
		}
		defer ts.Close()
		if _, err := ts.Up(ctx); err != nil {
			log.Fatalf("tailscale up: %v", err)
		}
		lc, err := ts.LocalClient()
		if err != nil {
			log.Fatalf("local client: %v", err)
		}
		ln, err = ts.Listen("tcp", env("TAILTERM_LISTEN", ":80"))
		if err != nil {
			log.Fatalf("tailnet listen: %v", err)
		}
		identity = func(r *http.Request) (api.Caller, error) {
			who, err := lc.WhoIs(r.Context(), r.RemoteAddr)
			if err != nil {
				return api.Caller{}, err
			}
			c := api.Caller{}
			if who.Node != nil {
				c.Node = who.Node.ComputedName
				if c.Node == "" {
					c.Node = who.Node.Name
				}
			}
			if who.UserProfile != nil {
				c.User = who.UserProfile.LoginName
			}
			if c.Node == "" && c.User == "" {
				return c, errors.New("unknown peer")
			}
			return c, nil
		}
		status, err := lc.Status(ctx)
		if err == nil && status.Self != nil {
			log.Printf("tailnet node %s (%v) listening on %s", status.Self.DNSName, status.TailscaleIPs, env("TAILTERM_LISTEN", ":80"))
		}
	}

	srv := &http.Server{
		Handler:           server.New(st, identity),
		ReadHeaderTimeout: 10 * time.Second,
		// Long-polls wait up to 30 s; leave headroom.
		WriteTimeout: 45 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
