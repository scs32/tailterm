// Command tailterm-hub serves the task hub as its own Tailscale node.
//
// Environment:
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
	"syscall"
	"time"

	"tailscale.com/tsnet"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var ln net.Listener
	var identity server.Identity
	if dev := os.Getenv("TAILTERM_DEV_LISTEN"); dev != "" {
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
			Logf:     func(string, ...any) {},
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
