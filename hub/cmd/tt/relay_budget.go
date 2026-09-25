package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type budgetBucket struct {
	mu         sync.Mutex
	tokens     float64
	capacity   float64
	perSecond  float64
	lastRefill time.Time
}

func newBudgetBucket(perMinute, burst int, now time.Time) *budgetBucket {
	return &budgetBucket{tokens: float64(burst), capacity: float64(burst), perSecond: float64(perMinute) / 60, lastRefill: now}
}

func (b *budgetBucket) available(now time.Time) float64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return min(b.capacity, b.tokens+max(0, now.Sub(b.lastRefill).Seconds())*b.perSecond)
}

func (b *budgetBucket) take(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.mu.Lock()
		now := time.Now()
		if elapsed := now.Sub(b.lastRefill).Seconds(); elapsed > 0 {
			b.tokens = min(b.capacity, b.tokens+elapsed*b.perSecond)
			b.lastRefill = now
		}
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		wait := time.Duration((1-b.tokens)/b.perSecond*float64(time.Second)) + time.Millisecond
		b.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// One relay owns queue and binding HTTP traffic on this host. A parent bucket
// enforces the owner cap; separate queue/binding buckets reserve fair shares
// so a busy queue cannot consume every token before inbox and broker checks.
type relayRateBudget struct {
	mu      sync.RWMutex
	domain  string
	version int64
	parent  *budgetBucket
	queue   *budgetBucket
	binding *budgetBucket
}

var activeRelayBudget = new(relayRateBudget)

func (b *relayRateBudget) configure(p *api.TeamHostPolicy, now time.Time) error {
	if p == nil {
		return errors.New("host policy is missing")
	}
	expires, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil || !expires.After(now) || p.LimiterDomain == "" || p.MaxRequestsPerMinute < 1 || p.MaxBurst < 2 || p.HeadroomPercent < 1 || p.HeadroomPercent >= 100 {
		return errors.New("host policy has no current request budget")
	}
	effectiveRate := p.MaxRequestsPerMinute * (100 - p.HeadroomPercent) / 100
	effectiveBurst := p.MaxBurst * (100 - p.HeadroomPercent) / 100
	if effectiveRate < 2 || effectiveBurst < 2 {
		return errors.New("host policy request budget cannot be fairly shared")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.domain == p.LimiterDomain && b.version == p.Version {
		return nil
	}
	queueRate, queueBurst := max(1, effectiveRate/4), max(1, effectiveBurst/4)
	oldDomain, oldParent, oldQueue, oldBinding := b.domain, b.parent, b.queue, b.binding
	b.domain, b.version = p.LimiterDomain, p.Version
	b.parent = newBudgetBucket(effectiveRate, effectiveBurst, now)
	b.queue = newBudgetBucket(queueRate, queueBurst, now)
	b.binding = newBudgetBucket(effectiveRate-queueRate, effectiveBurst-queueBurst, now)
	if oldParent != nil && oldDomain == p.LimiterDomain {
		b.parent.tokens = min(b.parent.capacity, oldParent.available(now))
		b.queue.tokens = min(b.queue.capacity, oldQueue.available(now))
		b.binding.tokens = min(b.binding.capacity, oldBinding.available(now))
	}
	return nil
}

func (b *relayRateBudget) wait(ctx context.Context, domain, path string) error {
	b.mu.RLock()
	if b.parent == nil || b.domain != domain {
		b.mu.RUnlock()
		return nil // Legacy N=1 traffic has no parallel host policy.
	}
	parent, class := b.parent, b.binding
	if strings.Contains(path, "/team-queue") {
		class = b.queue
	}
	b.mu.RUnlock()
	if err := class.take(ctx); err != nil {
		return err
	}
	return parent.take(ctx)
}

type relayBudgetTransport struct {
	base   http.RoundTripper
	budget *relayRateBudget
}

func (t relayBudgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	domain, err := canonicalLimiterDomain(req.URL.Scheme + "://" + req.URL.Host)
	if err != nil {
		return nil, err
	}
	if err := t.budget.wait(req.Context(), domain, req.URL.Path); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func attachRelayBudget(c *api.Client, budget *relayRateBudget) {
	if c == nil || budget == nil || c.HTTP == nil {
		return
	}
	if _, ok := c.HTTP.Transport.(relayBudgetTransport); ok {
		return
	}
	base := c.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.HTTP.Transport = relayBudgetTransport{base: base, budget: budget}
}
