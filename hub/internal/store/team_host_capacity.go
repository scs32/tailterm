package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func validLimiterDomain(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && raw == u.Scheme+"://"+strings.ToLower(u.Host)
}

func readTeamHostPolicy(ctx context.Context, q queryRower, host string) (*api.TeamHostPolicy, error) {
	var p api.TeamHostPolicy
	err := q.QueryRowContext(ctx, `SELECT host,limiter_domain,version,expires_at,max_sessions,max_polling,max_relay_bindings,max_requests_per_minute,max_burst,headroom_percent FROM team_host_policies WHERE host=?`, host).Scan(&p.Host, &p.LimiterDomain, &p.Version, &p.ExpiresAt, &p.MaxSessions, &p.MaxPolling, &p.MaxRelayBindings, &p.MaxRequestsPerMinute, &p.MaxBurst, &p.HeadroomPercent)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

func readTeamHostUsage(ctx context.Context, q queryRower, host string) (*api.TeamHostUsage, error) {
	var u api.TeamHostUsage
	err := q.QueryRowContext(ctx, `SELECT host,limiter_domain,policy_version,observed_at,relay_bindings,complete,source_digest FROM team_host_usage WHERE host=?`, host).Scan(&u.Host, &u.LimiterDomain, &u.PolicyVersion, &u.ObservedAt, &u.RelayBindings, &u.Complete, &u.SourceDigest)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &u, err
}

// Capacity is owner supplied and expires. Conservative reservations include
// uncertain launches and every non-cleaned exact agent run on the host.
func checkTeamHostCapacity(ctx context.Context, tx *sql.Tx, host string, now time.Time, additionalReservations int) error {
	policy, err := readTeamHostPolicy(ctx, tx, host)
	if err != nil {
		return err
	}
	if policy == nil {
		return fmt.Errorf("%w: host capacity policy is missing", api.ErrConflict)
	}
	until, err := time.Parse(time.RFC3339, policy.ExpiresAt)
	if err != nil || policy.Version < 1 || !until.After(now) || !validLimiterDomain(policy.LimiterDomain) || policy.MaxRelayBindings < 1 || policy.MaxRequestsPerMinute < 1 || policy.MaxBurst < 1 || policy.HeadroomPercent < 1 || policy.HeadroomPercent >= 100 {
		return fmt.Errorf("%w: host capacity policy is stale", api.ErrConflict)
	}
	usage, err := readTeamHostUsage(ctx, tx, host)
	if err != nil {
		return err
	}
	if usage == nil || !usage.Complete || usage.LimiterDomain != policy.LimiterDomain || usage.PolicyVersion != policy.Version || !validContextDigest(usage.SourceDigest) || usage.RelayBindings < 0 {
		return fmt.Errorf("%w: complete host relay binding census is missing", api.ErrConflict)
	}
	observed, err := time.Parse(time.RFC3339Nano, usage.ObservedAt)
	if err != nil || observed.After(now.Add(5*time.Second)) || now.Sub(observed) > 30*time.Second {
		return fmt.Errorf("%w: host relay binding census is stale", api.ErrConflict)
	}
	var agents, pendingSlots, polls int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE host=? AND (status<>'closed' OR cleanup_done=0)`, host).Scan(&agents); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(q.launch_json,'') FROM team_launch_reservations r LEFT JOIN team_queue_entries q ON q.id=r.entry_id WHERE r.state IN ('reserved','launching') AND ((r.entry_id='' AND (r.host=? OR r.host='')) OR (r.entry_id<>'' AND q.host=?))`, host, host)
	if err != nil {
		return err
	}
	for rows.Next() {
		var frozen string
		if err := rows.Scan(&frozen); err != nil {
			rows.Close()
			return err
		}
		if frozen == "" {
			pendingSlots += 4
			continue
		}
		var plan struct {
			Members []struct {
				State string `json:"state"`
			} `json:"members"`
		}
		if json.Unmarshal([]byte(frozen), &plan) != nil || len(plan.Members) == 0 {
			rows.Close()
			return fmt.Errorf("%w: frozen reservation is invalid", api.ErrConflict)
		}
		for _, member := range plan.Members {
			if member.State != "started" {
				pendingSlots++
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (
		SELECT task_id FROM agents WHERE host=? AND (status<>'closed' OR cleanup_done=0)
		UNION SELECT task_id FROM team_queue_entries WHERE host=? AND state IN ('queued','launching','running'))`, host, host).Scan(&polls); err != nil {
		return err
	}
	projectedSessions := agents + pendingSlots + 4*additionalReservations
	projectedBindings := max(usage.RelayBindings, agents) + pendingSlots + 4*additionalReservations
	// relayOne reads project and agent every three seconds; the broker path
	// adds up to three reads/writes per ten-second check; message pages and
	// queue effects add bounded slack. These are admission costs, while the
	// transport token bucket below enforces actual requests.
	bindingRate := 80 * projectedBindings
	queueRate := 20 * (1 + 6*polls)
	bindingBurst := 6 * projectedBindings
	queueBurst := 6*polls + 1
	rateBudget := policy.MaxRequestsPerMinute * (100 - policy.HeadroomPercent) / 100
	burstBudget := policy.MaxBurst * (100 - policy.HeadroomPercent) / 100
	queueRateBudget := max(1, rateBudget/4)
	queueBurstBudget := max(1, burstBudget/4)
	if projectedSessions > policy.MaxSessions || polls > policy.MaxPolling || projectedBindings > policy.MaxRelayBindings || queueRate > queueRateBudget || bindingRate > rateBudget-queueRateBudget || queueBurst > queueBurstBudget || bindingBurst > burstBudget-queueBurstBudget {
		return fmt.Errorf("%w: host session or polling budget exhausted", api.ErrConflict)
	}
	return nil
}
