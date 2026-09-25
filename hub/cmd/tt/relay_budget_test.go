package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type budgetRoundTripFunc func(*http.Request) (*http.Response, error)

func (f budgetRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestHostRelayCensusRequiresEveryBindingReadableAndOneDomain(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	domain := "https://hub.example.invalid"
	write := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".binding.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("one", runtimeBinding{Hub: domain, Task: "tsk_aaaaaaaaaaaaaaaa", Agent: "agt_aaaaaaaaaaaaaaaa", Run: "run_aaaaaaaaaaaaaaaa", Thread: "11111111-1111-1111-1111-111111111111", Codex: "/usr/bin/false"})
	usage := hostRelayCensus(dir, "mini", domain, now)
	if !usage.Complete || usage.RelayBindings != 1 || len(usage.SourceDigest) != 64 {
		t.Fatalf("valid census %+v", usage)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.binding.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	usage = hostRelayCensus(dir, "mini", domain, now)
	if usage.Complete || usage.RelayBindings != 1 {
		t.Fatalf("unreadable binding was ignored: %+v", usage)
	}
	if err := os.Remove(filepath.Join(dir, "broken.binding.json")); err != nil {
		t.Fatal(err)
	}
	write("other", runtimeBinding{Hub: "https://other.example.invalid", Task: "tsk_bbbbbbbbbbbbbbbb", Agent: "agt_bbbbbbbbbbbbbbbb", Run: "run_bbbbbbbbbbbbbbbb", Thread: "22222222-2222-2222-2222-222222222222", Codex: "/usr/bin/false"})
	usage = hostRelayCensus(dir, "mini", domain, now)
	if usage.Complete || usage.RelayBindings != 1 {
		t.Fatalf("other limiter domain was ignored: %+v", usage)
	}
}

func TestRelayBudgetReservesBindingShareWhenQueueBursts(t *testing.T) {
	var budget relayRateBudget
	now := time.Now().UTC()
	if err := budget.configure(&api.TeamHostPolicy{LimiterDomain: "https://hub.example.invalid", Version: 1, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), MaxRequestsPerMinute: 120, MaxBurst: 4, HeadroomPercent: 50}, now); err != nil {
		t.Fatal(err)
	}
	if err := budget.wait(context.Background(), "https://hub.example.invalid", "/v1/team-queues"); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := budget.wait(short, "https://hub.example.invalid", "/v1/team-queues"); err == nil {
		t.Fatal("queue exceeded reserved burst share")
	}
	if err := budget.wait(context.Background(), "https://hub.example.invalid", "/v1/tasks/tsk_aaaaaaaaaaaaaaaa/agents"); err != nil {
		t.Fatalf("queue burst starved binding path: %v", err)
	}
	short2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if err := budget.wait(short2, "https://hub.example.invalid", "/v1/tasks/tsk_aaaaaaaaaaaaaaaa/agents"); err == nil {
		t.Fatal("shared parent burst was exceeded")
	}
}

func TestRelayHTTPTransportChargesQueueAndBindingRequests(t *testing.T) {
	var budget relayRateBudget
	now := time.Now().UTC()
	if err := budget.configure(&api.TeamHostPolicy{LimiterDomain: "https://hub.example.invalid", Version: 1, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), MaxRequestsPerMinute: 120, MaxBurst: 4, HeadroomPercent: 50}, now); err != nil {
		t.Fatal(err)
	}
	requests := 0
	c := &api.Client{HTTP: &http.Client{Transport: budgetRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})}}
	attachRelayBudget(c, &budget)
	for _, path := range []string{"/v1/team-queues", "/v1/tasks/tsk_aaaaaaaaaaaaaaaa/agents"} {
		req, err := http.NewRequest("GET", "https://hub.example.invalid"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := c.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://hub.example.invalid/v1/team-queues", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.HTTP.Do(req); err == nil || requests != 2 {
		t.Fatalf("transport bypassed shared limiter: requests=%d err=%v", requests, err)
	}
}

func TestRelayBudgetPolicyRevisionDoesNotRefillBurst(t *testing.T) {
	var budget relayRateBudget
	now := time.Now().UTC()
	policy := &api.TeamHostPolicy{LimiterDomain: "https://hub.example.invalid", Version: 1, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339), MaxRequestsPerMinute: 120, MaxBurst: 4, HeadroomPercent: 50}
	if err := budget.configure(policy, now); err != nil {
		t.Fatal(err)
	}
	if err := budget.wait(context.Background(), policy.LimiterDomain, "/v1/team-queues"); err != nil {
		t.Fatal(err)
	}
	policy.Version++
	if err := budget.configure(policy, now); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := budget.wait(ctx, policy.LimiterDomain, "/v1/team-queues"); err == nil {
		t.Fatal("policy revision refilled an exhausted queue burst")
	}
}
