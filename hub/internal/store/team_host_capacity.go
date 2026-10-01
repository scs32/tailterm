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
	err := q.QueryRowContext(ctx, `SELECT host,limiter_domain,version,expires_at,max_sessions,max_polling,max_relay_bindings,max_requests_per_minute,max_burst,headroom_percent,min_free_disk_mib FROM team_host_policies WHERE host=?`, host).Scan(&p.Host, &p.LimiterDomain, &p.Version, &p.ExpiresAt, &p.MaxSessions, &p.MaxPolling, &p.MaxRelayBindings, &p.MaxRequestsPerMinute, &p.MaxBurst, &p.HeadroomPercent, &p.MinFreeDiskMiB)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

func readTeamHostUsage(ctx context.Context, q queryRower, host string) (*api.TeamHostUsage, error) {
	var u api.TeamHostUsage
	var free sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT host,limiter_domain,policy_version,observed_at,relay_bindings,complete,source_digest,free_disk_mib FROM team_host_usage WHERE host=?`, host).Scan(&u.Host, &u.LimiterDomain, &u.PolicyVersion, &u.ObservedAt, &u.RelayBindings, &u.Complete, &u.SourceDigest, &free)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if free.Valid {
		u.FreeDiskMiB = &free.Int64
	}
	return &u, err
}

func sameFreeDisk(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// checkTeamHostAdmission gates a new parallel team: host capacity for one
// more reservation and the free-disk reserve. A launch already admitted
// checks capacity alone, so low disk never strands a half-started team.
func checkTeamHostAdmission(ctx context.Context, tx *sql.Tx, host string, now time.Time) error {
	if err := checkTeamHostCapacity(ctx, tx, host, now, 1); err != nil {
		return err
	}
	policy, err := readTeamHostPolicy(ctx, tx, host)
	if err != nil {
		return err
	}
	usage, err := readTeamHostUsage(ctx, tx, host)
	if err != nil {
		return err
	}
	if usage.FreeDiskMiB == nil {
		return fmt.Errorf("%w: host free disk is not observed", api.ErrConflict)
	}
	if reserve := policy.DiskReserveMiB(); *usage.FreeDiskMiB < reserve {
		return fmt.Errorf("%w: host free disk %d MiB is below the %d MiB reserve", api.ErrConflict, *usage.FreeDiskMiB, reserve)
	}
	return nil
}

// Planned delivery team sizes: features get a plan reviewer; bugs get a plan
// only (wi_ade4aa60c5d9b55e). The database seat is never launched.
const (
	featureTeamSlots = 6
	bugTeamSlots     = 5
)

// Capacity is owner supplied and expires. Conservative reservations include
// uncertain launches and every non-cleaned exact agent run on the host. A
// reservation without a frozen plan charges its item's team size (a feature
// when the kind is unknown); the next admission has no item yet, so it charges
// the larger feature team.
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
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(q.launch_json,''),COALESCE(w.kind,'') FROM team_launch_reservations r LEFT JOIN team_queue_entries q ON q.id=r.entry_id LEFT JOIN work_items w ON w.id=r.item_id WHERE r.state IN ('reserved','launching') AND ((r.entry_id='' AND (r.host=? OR r.host='')) OR (r.entry_id<>'' AND q.host=?))`, host, host)
	if err != nil {
		return err
	}
	for rows.Next() {
		var frozen, kind string
		if err := rows.Scan(&frozen, &kind); err != nil {
			rows.Close()
			return err
		}
		if frozen == "" {
			if kind == "bug" {
				pendingSlots += bugTeamSlots
			} else {
				pendingSlots += featureTeamSlots
			}
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
	projectedSessions := agents + pendingSlots + featureTeamSlots*additionalReservations
	projectedBindings := max(usage.RelayBindings, agents) + pendingSlots + featureTeamSlots*additionalReservations
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

// smallTeamSlots is the small-change lane's team size. Host capacity keeps
// charging an unfrozen small team as a bug team; only the project agent cap
// uses this.
const smallTeamSlots = 3

// projectAgentCapPrefix starts every project agent cap reason. The queue
// runner matches on it to hold an entry instead of failing it.
const projectAgentCapPrefix = "project agent cap: "

// queueTeamSeats is the number of agents an unfrozen entry's team registers.
func queueTeamSeats(kind, template string) int {
	if template == "small" {
		return smallTeamSlots
	}
	if kind == "bug" {
		return bugTeamSlots
	}
	return featureTeamSlots
}

// projectAgentCapUsage counts what already stands against a project's agent
// cap. Open is exactly what agent registration counts: every agent of the
// project, persistent roles and done or retired agents included, whose status
// is not closed or exited. Reserved is the seats admitted launches still have
// to register: the members of each launching entry's frozen plan that are not
// started (its team size before the freeze), plus the team of each manual
// reservation. An uncertain member may already be registered, so reserved can
// overcount by one per launch.
func projectAgentCapUsage(ctx context.Context, q queryRower, task string) (open, reserved int, err error) {
	if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE task_id=? AND status NOT IN ('closed','exited')`, task).Scan(&open); err != nil {
		return 0, 0, err
	}
	rows, err := q.QueryContext(ctx, `SELECT COALESCE(e.launch_json,''),e.template,COALESCE(w.kind,'') FROM team_queue_entries e LEFT JOIN work_items w ON w.id=e.item_id WHERE e.task_id=? AND e.state='launching'
 UNION ALL SELECT '','',COALESCE(w.kind,'') FROM team_launch_reservations r LEFT JOIN work_items w ON w.id=r.item_id WHERE r.task_id=? AND r.entry_id='' AND r.state IN ('reserved','launching')`, task, task)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var frozen, template, kind string
		if err = rows.Scan(&frozen, &template, &kind); err != nil {
			return 0, 0, err
		}
		if frozen == "" {
			reserved += queueTeamSeats(kind, template)
			continue
		}
		var plan struct {
			Members []struct {
				State string `json:"state"`
			} `json:"members"`
		}
		if json.Unmarshal([]byte(frozen), &plan) != nil || len(plan.Members) == 0 {
			return 0, 0, fmt.Errorf("%w: frozen reservation is invalid", api.ErrConflict)
		}
		for _, member := range plan.Members {
			if member.State != "started" {
				reserved++
			}
		}
	}
	return open, reserved, rows.Err()
}

// projectAgentCapReason names why seats more agents do not fit under the cap,
// or is empty when they fit.
func projectAgentCapReason(open, reserved, seats, maxAgents int) string {
	if open+reserved+seats <= maxAgents {
		return ""
	}
	if reserved > 0 {
		return fmt.Sprintf("%s%d open + %d reserved + %d seats > %d", projectAgentCapPrefix, open, reserved, seats, maxAgents)
	}
	return fmt.Sprintf("%s%d open + %d seats > %d", projectAgentCapPrefix, open, seats, maxAgents)
}

// checkProjectAgentCap refuses a team that agent registration would refuse
// partway through its launch (docs/project-queue.md, "Project agent cap").
// maxAgents is the store's enforced cap, not the compiled default.
func checkProjectAgentCap(ctx context.Context, q queryRower, task string, maxAgents, seats int) error {
	open, reserved, err := projectAgentCapUsage(ctx, q, task)
	if err != nil {
		return err
	}
	if reason := projectAgentCapReason(open, reserved, seats, maxAgents); reason != "" {
		return fmt.Errorf("%w: %s", api.ErrConflict, reason)
	}
	return nil
}

// queueItemKind reads an item's kind; a missing item is an unknown kind.
func queueItemKind(ctx context.Context, q queryRower, task, item string) (string, error) {
	var kind string
	err := q.QueryRowContext(ctx, `SELECT kind FROM work_items WHERE task_id=? AND id=?`, task, item).Scan(&kind)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return kind, err
}
