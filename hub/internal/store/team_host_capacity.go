package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	var free, pressure, waiters sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT host,limiter_domain,policy_version,observed_at,relay_bindings,complete,source_digest,free_disk_mib,memory_pressure,matrix_waiters FROM team_host_usage WHERE host=?`, host).Scan(&u.Host, &u.LimiterDomain, &u.PolicyVersion, &u.ObservedAt, &u.RelayBindings, &u.Complete, &u.SourceDigest, &free, &pressure, &waiters)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if free.Valid {
		u.FreeDiskMiB = &free.Int64
	}
	u.MemoryPressure, u.MatrixWaiters = nullInt(pressure), nullInt(waiters)
	return &u, err
}

func nullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func intOrNull(v *int) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

func sameInt(a, b *int) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
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

// handlerSuccessorSeat is the seat queue admission and automatic handler
// provisioning hold for the primary handler's successor (bugs
// wi_31ec140ea47e3441, wi_bbea3ea3ad6068a1): 1 while a prepared rotation
// waits for its successor to register, or, with none prepared, while the
// current primary run has a successor hold that counts (successorHoldCounts),
// or while the rotation is due by its items or tokens limit,
// provider_blocked, or a dead primary whose silence is met; otherwise 0. Due
// is HandlerRotationsDue's own rule, read through q, without the runner's
// template digest: a template rotation holds the seat only through the hold
// its refused prepare recorded. A rotation's prepare never counts this seat:
// it is that seat.
func handlerSuccessorSeat(ctx context.Context, q queryRower, task string, now time.Time) (int, error) {
	r, err := openHandlerRotation(ctx, q, task)
	if err != nil {
		return 0, err
	}
	if r != nil {
		var registered int
		if err = q.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE task_id=? AND id=? AND status NOT IN ('closed','exited')`, task, r.SuccessorAgentID).Scan(&registered); err != nil {
			return 0, err
		}
		if registered > 0 {
			return 0, nil
		}
		return 1, nil
	}
	t, err := scanTask(q.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if t.Status != api.TaskOpen || t.PauseState != api.ProjectPauseActive {
		return 0, nil
	}
	policy, err := loadHandlerRotationPolicy(ctx, q, task)
	if err != nil {
		return 0, err
	}
	if hold, err := loadSuccessorHold(ctx, q, t); err != nil {
		return 0, err
	} else if hold != nil && successorHoldCounts(*hold, policy) {
		return 1, nil
	}
	d, listed, err := primaryRotationDue(ctx, q, t, policy, "", "", now)
	if err != nil || !listed || (len(d.DueReasons) == 0 && !d.SilenceMet) {
		return 0, err
	}
	return 1, nil
}

// queueAgentCapReason is projectAgentCapReason for queue admission, which
// also counts held handler successor seats. With none held its text is
// projectAgentCapReason's.
func queueAgentCapReason(open, reserved, held, seats, maxAgents int) string {
	if held == 0 {
		return projectAgentCapReason(open, reserved, seats, maxAgents)
	}
	if open+reserved+held+seats <= maxAgents {
		return ""
	}
	counts := fmt.Sprintf("%d open", open)
	if reserved > 0 {
		counts += fmt.Sprintf(" + %d reserved", reserved)
	}
	return fmt.Sprintf("%s%s + %d seat held for the handler successor + %d seats > %d", projectAgentCapPrefix, counts, held, seats, maxAgents)
}

// checkProjectAgentCap refuses a team that agent registration would refuse
// partway through its launch, or that would take the seat held for a due
// handler successor (docs/project-queue.md, "Project agent cap"). maxAgents
// is the store's enforced cap, not the compiled default.
func checkProjectAgentCap(ctx context.Context, q queryRower, task string, maxAgents, seats int, now time.Time) error {
	open, reserved, err := projectAgentCapUsage(ctx, q, task)
	if err != nil {
		return err
	}
	held, err := handlerSuccessorSeat(ctx, q, task, now)
	if err != nil {
		return err
	}
	if reason := queueAgentCapReason(open, reserved, held, seats, maxAgents); reason != "" {
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

// maxQueueLanes bounds a lane count; the concurrency limit, when fixed, is
// the real ceiling.
const maxQueueLanes = 64

// validQueueLanePolicy checks a lane policy against the project's limit and
// returns it with its parsed window. The limit stays the ceiling, so a lane
// count never admits more than the limit already allowed and no host or cap
// gate needs to run again.
func validQueueLanePolicy(p api.TeamQueueLanePolicy, limit int) (releaseWindow, error) {
	invalid := func(why string) (releaseWindow, error) {
		return releaseWindow{}, fmt.Errorf("%w: lane policy: %s", api.ErrInvalid, why)
	}
	if !queueParallel(limit) {
		return invalid("the queue is serial (limit 1); lanes need a parallel limit: tt team queue limit --limit N")
	}
	if p.Day < 1 || p.Night < 1 || p.Day > maxQueueLanes || p.Night > maxQueueLanes {
		return invalid(fmt.Sprintf("day and night lanes are 1 to %d", maxQueueLanes))
	}
	if limit > 0 && (p.Day > limit || p.Night > limit) {
		return invalid(fmt.Sprintf("day %d and night %d lanes must not exceed the concurrency limit %d", p.Day, p.Night, limit))
	}
	if p.Zone == "" {
		return invalid("zone required, an IANA name such as America/Los_Angeles")
	}
	loc, err := interventionLocation(p.Zone)
	if err != nil {
		return invalid("unknown zone " + strconv.Quote(p.Zone) + "; use an IANA name such as America/Los_Angeles")
	}
	start, ok := releaseWindowClock(p.NightStart)
	end, ok2 := releaseWindowClock(p.NightEnd)
	if !ok || !ok2 {
		return invalid("night start and end are 24-hour HH:MM, 00:00 to 23:59")
	}
	if start == end {
		return invalid("night start and end must differ")
	}
	if p.StrainPressure < 1 || p.StrainPressure > 99 {
		return invalid("strain pressure is a whole number from 1 to 99")
	}
	if p.StrainLanes < 0 || p.StrainLanes >= min(p.Day, p.Night) {
		return invalid(fmt.Sprintf("strain lanes are 0 to %d, fewer than the smaller lane count", min(p.Day, p.Night)-1))
	}
	return releaseWindow{loc: loc, start: start, end: end}, nil
}

// loadQueueLanePolicy reads a project's lane policy, or nil when it has none.
func loadQueueLanePolicy(ctx context.Context, q queryRower, task string) (*api.TeamQueueLanePolicy, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT lane_policy_json FROM team_queue_settings WHERE task_id=?`, task).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && raw == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p api.TeamQueueLanePolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// queueLanes is the lane policy as it applies at now: the day or night lane
// count, StrainLanes fewer (never fewer than one) while the census's memory
// pressure is over the threshold, and a hold while a matrix run waits or the
// lock is not observed. Unobserved pressure is not strain. A fixed limit
// stays the ceiling. Active is left for the caller.
func queueLanes(limit int, p api.TeamQueueLanePolicy, u *api.TeamHostUsage, now time.Time) api.TeamQueueLanes {
	out := api.TeamQueueLanes{Policy: p, Window: api.LaneWindowDay, Lanes: p.Day}
	if w, err := validQueueLanePolicy(p, 0); err == nil && releaseWindowInside(w, now) {
		out.Window, out.Lanes = api.LaneWindowNight, p.Night
	}
	if u != nil {
		out.MemoryPressure, out.MatrixWaiters = u.MemoryPressure, u.MatrixWaiters
	}
	if out.MemoryPressure != nil && *out.MemoryPressure > p.StrainPressure {
		out.Strained = true
		out.Lanes = max(1, out.Lanes-p.StrainLanes)
	}
	if limit > 0 {
		out.Lanes = min(out.Lanes, limit)
	}
	out.ScheduleState = hostScheduleState(p.HostSchedule, now)
	if p.HoldWhileMatrixWaits {
		switch {
		case out.MatrixWaiters == nil:
			out.Hold = "the verification host's matrix lock is not observed"
		case *out.MatrixWaiters > 0:
			out.Hold = fmt.Sprintf("a matrix run waits for the verification host (%d waiting)", *out.MatrixWaiters)
		}
	}
	return out
}

// queueLanesWhy names what set the lane count, as the listing and the
// claim's refusal say it: the window, and strain when it lowered the count.
func queueLanesWhy(l api.TeamQueueLanes) string {
	if l.Strained {
		return fmt.Sprintf("%s, memory pressure %d over %d", l.Window, *l.MemoryPressure, l.Policy.StrainPressure)
	}
	return l.Window
}

// queueLaneRefusal is the claim's refusal when lanes admit no team now, or
// nil. Both texts end as the limit's own, so the runner waits silently.
func queueLaneRefusal(l api.TeamQueueLanes, active int) error {
	if l.Hold != "" {
		return fmt.Errorf("%w: lane policy holds admission: %s; all team slots are reserved", api.ErrConflict, l.Hold)
	}
	if active >= l.Lanes {
		return fmt.Errorf("%w: lane policy admits %d teams (%s); all team slots are reserved", api.ErrConflict, l.Lanes, queueLanesWhy(l))
	}
	return nil
}

// queueLaneReason is the listing's reason for a queued entry the lanes hold,
// or empty.
func queueLaneReason(l api.TeamQueueLanes, active int) string {
	if l.Hold != "" {
		return "Held: " + l.Hold
	}
	if active >= l.Lanes {
		return fmt.Sprintf("Lanes full: %d of %d (%s)", active, l.Lanes, queueLanesWhy(l))
	}
	return ""
}

// hostScheduleState is a host schedule as it applies at now: cleared once
// cleared, expired from its expiry on, else active; empty without one.
func hostScheduleState(h *api.TeamHostSchedule, now time.Time) string {
	switch {
	case h == nil:
		return ""
	case h.ClearedAt != "":
		return api.HostScheduleCleared
	}
	until, err := time.Parse(time.RFC3339Nano, h.ExpiresAt)
	if err != nil || !now.Before(until) {
		return api.HostScheduleExpired
	}
	return api.HostScheduleActive
}

// hostScheduleAuthority admits the owner (no agent fields), a live database
// handler run of the project, or the project's current owner helper, and
// returns who acts: the handler's or helper's agent ID, or "owner". An item
// lead is refused, as is a handler that is closed, exited or retired.
func hostScheduleAuthority(ctx context.Context, tx *sql.Tx, task string, req api.TeamQueueRequest) (string, error) {
	refused := fmt.Errorf("%w: only the owner, the owner helper or a database handler may change the host schedule; ask the database handler", api.ErrConflict)
	if req.LeadAgentID != "" || req.LeadRunID != "" {
		return "", refused
	}
	handler := req.HandlerAgentID != "" || req.HandlerRunID != ""
	if handler && req.AgentID != "" {
		return "", refused
	}
	if req.AgentID != "" {
		helper, ok, err := currentOwnerHelper(ctx, tx, task)
		if err != nil {
			return "", err
		}
		if !ok || helper.ID != req.AgentID || helper.Status == api.AgentExited || helper.Status == api.AgentRetired {
			return "", refused
		}
		return req.AgentID, nil
	}
	if !handler {
		return "owner", nil
	}
	return queueWaitAuthority(ctx, tx, task, req)
}

// validHostScheduleReason checks a one-line reason of at most
// api.HostScheduleMaxReason bytes; required says whether it may be empty.
func validHostScheduleReason(reason string, required bool) error {
	if len(reason) > api.HostScheduleMaxReason || strings.ContainsFunc(reason, unicode.IsControl) {
		return fmt.Errorf("%w: a host schedule reason is one line of at most %d bytes", api.ErrInvalid, api.HostScheduleMaxReason)
	}
	if required && strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: a host schedule needs --reason TEXT saying why matrix starts take turns", api.ErrInvalid)
	}
	return nil
}

// teamHostScheduleAction runs schedule_set, schedule_grant, schedule_revoke
// and schedule_clear inside the queue action's transaction. The schedule is
// a field of the stored lane policy, so it needs one: a lanes save keeps it
// and lanes --off is refused while it is active. set replaces any earlier
// schedule and drops its grants; clear keeps the record with who cleared it,
// when and why, and drops the grants. Grants and revokes need an active
// schedule. Each change refreshes the policy's UpdatedAt.
func teamHostScheduleAction(ctx context.Context, tx *sql.Tx, task string, req api.TeamQueueRequest, now time.Time) error {
	policy, err := loadQueueLanePolicy(ctx, tx, task)
	if err != nil {
		return err
	}
	if policy == nil {
		return fmt.Errorf("%w: a host schedule needs a lane policy; set one first: tt team queue lanes --task %s --day N --night N --zone ZONE --night-start HH:MM --night-end HH:MM", api.ErrConflict, task)
	}
	by, err := hostScheduleAuthority(ctx, tx, task, req)
	if err != nil {
		return err
	}
	at := ts(now)
	reason := strings.TrimSpace(req.ScheduleReason)
	state := hostScheduleState(policy.HostSchedule, now)
	switch req.Operation {
	case "schedule_set":
		if req.ScheduleItem != "" {
			return fmt.Errorf("%w: schedule set takes no item", api.ErrInvalid)
		}
		if err := validHostScheduleReason(req.ScheduleReason, true); err != nil {
			return err
		}
		if req.ScheduleTTLMinutes < 1 || req.ScheduleTTLMinutes > api.HostScheduleMaxTTLMinutes {
			return fmt.Errorf("%w: a host schedule expires in 1 to %d minutes", api.ErrInvalid, api.HostScheduleMaxTTLMinutes)
		}
		policy.HostSchedule = &api.TeamHostSchedule{Reason: reason, SetBy: by, SetAt: at, ExpiresAt: ts(now.Add(time.Duration(req.ScheduleTTLMinutes) * time.Minute))}
	case "schedule_grant", "schedule_revoke":
		if !api.ValidID(req.ScheduleItem, "wi") || req.ScheduleTTLMinutes != 0 {
			return fmt.Errorf("%w: a host turn grant or revoke names one item, --item wi_ID, and no expiry", api.ErrInvalid)
		}
		if err := validHostScheduleReason(req.ScheduleReason, false); err != nil {
			return err
		}
		if state != api.HostScheduleActive {
			return fmt.Errorf("%w: no host schedule is active, so matrix runs need no grant", api.ErrConflict)
		}
		h := *policy.HostSchedule
		grants := make([]api.TeamHostTurnGrant, 0, len(h.Grants)+1)
		found := false
		for _, g := range h.Grants {
			if g.ItemID == req.ScheduleItem {
				found = true
				continue
			}
			grants = append(grants, g)
		}
		if req.Operation == "schedule_revoke" {
			if !found {
				return fmt.Errorf("%w: %s holds no host turn grant", api.ErrNotFound, req.ScheduleItem)
			}
		} else {
			var kind string
			if err := tx.QueryRowContext(ctx, `SELECT kind FROM work_items WHERE task_id=? AND id=?`, task, req.ScheduleItem).Scan(&kind); errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: work item %s is not in this project", api.ErrNotFound, req.ScheduleItem)
			} else if err != nil {
				return err
			}
			grants = append(grants, api.TeamHostTurnGrant{ItemID: req.ScheduleItem, By: by, At: at, Reason: reason})
		}
		if len(grants) == 0 {
			grants = nil
		}
		h.Grants = grants
		policy.HostSchedule = &h
	case "schedule_clear":
		if req.ScheduleItem != "" || req.ScheduleTTLMinutes != 0 {
			return fmt.Errorf("%w: schedule clear takes only --reason", api.ErrInvalid)
		}
		if err := validHostScheduleReason(req.ScheduleReason, true); err != nil {
			return err
		}
		if state != api.HostScheduleActive && state != api.HostScheduleExpired {
			return fmt.Errorf("%w: no host schedule is set to clear", api.ErrNotFound)
		}
		h := *policy.HostSchedule
		h.Grants, h.ClearedBy, h.ClearedAt, h.ClearReason = nil, by, at, reason
		policy.HostSchedule = &h
	default:
		return api.ErrInvalid
	}
	policy.UpdatedAt = at
	b, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE team_queue_settings SET lane_policy_json=? WHERE task_id=?`, string(b), task)
	return err
}
