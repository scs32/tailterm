package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The stall watch (docs/stall-watch.md). One sweep on the broker tick opens a
// durable episode and posts one notice when the owner helper has been quiet
// for 20 minutes while it owes the owner something, or when the team of a
// running queue entry has been quiet for 30 minutes. It alerts only: it types
// into no pane, wakes, restarts, closes or resumes no agent beyond the
// ordinary delivery of its one directed notice, and changes no queue or
// work-item state. The sweep writes its notices and stall_watch_episodes;
// stall_watch_progress is written only by the activity report, which also
// posts the one directed notice that tells an agent its own matrix run ended.

func migrateStallWatch(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS stall_watch_progress (
 agent_id TEXT NOT NULL, run_id TEXT NOT NULL, task_id TEXT NOT NULL,
 last_working_at TEXT NOT NULL, matrix_wait INTEGER NOT NULL DEFAULT 0,
 wait_ended_at TEXT NOT NULL DEFAULT '', PRIMARY KEY(agent_id,run_id));
CREATE TABLE IF NOT EXISTS stall_watch_episodes (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('helper','team')),
 subject_id TEXT NOT NULL,
 run_id TEXT NOT NULL DEFAULT '',
 item_id TEXT NOT NULL DEFAULT '',
 activity TEXT NOT NULL DEFAULT '',
 quiet_since TEXT NOT NULL, opened_at TEXT NOT NULL,
 closed_at TEXT NOT NULL DEFAULT '', close_reason TEXT NOT NULL DEFAULT '',
 recipient TEXT NOT NULL CHECK(recipient IN ('owner','helper')),
 owner_state TEXT NOT NULL DEFAULT '' CHECK(owner_state IN ('','deferred','pinged')),
 notice_seq INTEGER NOT NULL DEFAULT 0,
 ping_seq INTEGER NOT NULL DEFAULT 0,
 scanned_seq INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS stall_watch_episodes_open
 ON stall_watch_episodes(task_id,kind,subject_id) WHERE closed_at='';`)
	return err
}

const (
	stallWatchHelperQuiet = 20 * time.Minute
	stallWatchTeamQuiet   = 30 * time.Minute
	// A post older than this cannot stop either watch from opening.
	stallWatchPostWindow = 30 * time.Minute
	// Rows one episode read takes from the messages after its cursor.
	stallWatchScanLimit = 500
	// How long after a hub start an agent that has not heartbeated since is
	// not judged: two broker ticks, so the second sweep does not race the
	// first 30 s heartbeat.
	stallWatchStartHold = 60 * time.Second

	stallKindHelper = "helper"
	stallKindTeam   = "team"

	stallRecipientOwner  = "owner"
	stallRecipientHelper = "helper"

	stallOwnerDeferred = "deferred"
	stallOwnerPinged   = "pinged"

	stallActionHelper     = "helper-quiet"
	stallActionHelperPing = "helper-quiet-ping"
	stallActionTeam       = "team-quiet"
	stallActionTeamPing   = "team-quiet-ping"

	stallHelperSubject    = "The owner helper has gone quiet while it owes the owner"
	stallTeamSubject      = "A running team has gone quiet"
	stallTeamOwnerSubject = "A running team has gone quiet and needs the owner"

	stallUnknownCause = "The hub does not know why it stopped. It typed, restarted and resumed nothing."
	stallRecordFirst  = "Before resuming, record why it stopped (the recovery prime directive: cause, incident, prevention)."
	stallNextStep     = "Once that is recorded, the usual next step for this state is: "
	stallNightLine    = " The owner is pinged at 07:00 Pacific if this is still open."
)

// stallWatchBeforeWrite is nil in production. Tests set it to change state
// between the sweep's unlocked read and its write transactions.
var stallWatchBeforeWrite func()

// stallWatchStarts holds when each store's hub process started. Only the hub
// sets it; a store without one holds nothing.
var stallWatchStarts sync.Map // *Store -> time.Time

// StallWatchHubStarted records that the hub process started at at. For
// stallWatchStartHold after it the sweep does not judge an agent the hub has
// not heard from since.
func (s *Store) StallWatchHubStarted(at time.Time) { stallWatchStarts.Store(s, at.UTC()) }

// stallWatchStarted sets the recorded hub start, if any, on each view.
func (s *Store) stallWatchStarted(views map[string]*stallView) {
	at, ok := stallWatchStarts.Load(s)
	if !ok {
		return
	}
	for _, v := range views {
		v.started = at.(time.Time)
	}
}

// StallWatchStep is one notice a sweep posted.
type StallWatchStep struct {
	TaskID     string
	MessageSeq int64
	Action     string
}

var stallWatchPacific = sync.OnceValue(func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.FixedZone("PST", -8*60*60)
	}
	return loc
})

// stallWatchNight reports whether now falls in 22:00 to 07:00 Pacific, when
// no owner ping is sent.
func stallWatchNight(now time.Time) bool {
	hour := now.In(stallWatchPacific()).Hour()
	return hour >= 22 || hour < 7
}

// stallWatchActivityHook runs in the activity report transaction, which
// reaches it only when the report changed something. It keeps two times for a
// run: when it last worked (the report that enters working and the report
// that leaves it) and when its matrix wait last ended (the first report
// without the wait). Any other report changes no row. The report that ends a
// matrix wait, or replaces it with another run's, also posts that run's one
// notice to the reporting agent.
func (s *Store) stallWatchActivityHook(ctx context.Context, tx *sql.Tx, task, agent, run, oldState string, activity api.AgentActivity) error {
	observed := ts(activity.ObservedAt)
	waiting := activity.MatrixWait.Valid()
	var had bool
	if err := tx.QueryRowContext(ctx, `SELECT matrix_wait FROM stall_watch_progress WHERE agent_id=? AND run_id=?`, agent, run).Scan(&had); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !waiting {
		if _, err := tx.ExecContext(ctx, `UPDATE stall_watch_progress SET matrix_wait=0,wait_ended_at=? WHERE agent_id=? AND run_id=? AND matrix_wait=1`, observed, agent, run); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO stall_watch_progress(agent_id,run_id,task_id,last_working_at,matrix_wait) VALUES(?,?,?,'',1)
 ON CONFLICT(agent_id,run_id) DO UPDATE SET matrix_wait=1`, agent, run, task); err != nil {
		return err
	}
	if had {
		if err := s.postMatrixRunEnded(ctx, tx, task, agent, run, activity); err != nil {
			return err
		}
	}
	if activity.State != "working" && oldState != "working" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO stall_watch_progress(agent_id,run_id,task_id,last_working_at,matrix_wait) VALUES(?,?,?,?,?)
 ON CONFLICT(agent_id,run_id) DO UPDATE SET last_working_at=excluded.last_working_at`, agent, run, task, observed, waiting)
	return err
}

// The run-end notice (docs/stall-watch.md, "When a matrix run ends"). The hub
// is told only that a run left the host's matrix lock, so the text says where
// the receipt and the outcome are and never that the run passed.
const (
	matrixRunEndedSubject = "Your matrix run has ended"
	matrixRunEndedAction  = "matrix-run-ended"
	matrixRunEndedRunning = "was no longer in the host's matrix lock at %s: it finished, failed or was killed. " +
		"The hub is told only that it left, never a path or a result. " +
		"Read receipt.json (targeted-receipt.json for a targeted run) in the log directory you gave the run; its checks are the outcome. " +
		"If there is no receipt the run did not finish: read runner.json there for an exit code and host-lock.log for the reason, " +
		"and record why it stopped (the recovery rule in AGENTS.md) before anything else. "
	matrixRunEndedWaiting = "left the waitlist at %s without being seen running: it was withdrawn or killed, or it ran and ended inside one 15-second check. " +
		"The hub is told only that it left, never a path or a result. " +
		"Look for receipt.json (targeted-receipt.json for a targeted run) in the log directory you gave the run; its checks are the outcome. " +
		"With no receipt, read runner.json there for an exit code and host-lock.log for the reason, " +
		"and record why it stopped (the recovery rule in AGENTS.md) before anything else. "
	matrixRunEndedClose = "This notice does not authorise another run. It is your wake; you need no watcher and no message to yourself."
	// The report before the one being stored: the newest receipt of the run is
	// the current report, which ReportActivity inserts before its hooks.
	matrixRunEndedPrevious = `SELECT payload FROM agent_activity_receipts WHERE agent_id=? AND run_id=? ORDER BY rowid DESC LIMIT 1 OFFSET 1`
)

// matrixRunReplaced reports whether a report that still carries a wait
// carries another run's: the earlier run left the lock between two checks. A
// waiting run that gets the host is the same run.
func matrixRunReplaced(previous, current *api.MatrixWait) bool {
	switch {
	case previous.Item != "" && current.Item != "" && previous.Item != current.Item:
		return true
	case previous.Role == api.MatrixWaitRunning:
		return current.Role == api.MatrixWaitWaiting || !previous.Since.Equal(current.Since)
	}
	return current.Role == api.MatrixWaitWaiting && !previous.Since.Equal(current.Since)
}

// postMatrixRunEnded posts the run-end notice for the wait in the run's
// previous report, when the current report ends it or carries another run's.
// It is reached once per stored report whose run was in a wait, and a report
// is stored once, so a run gets one notice.
func (s *Store) postMatrixRunEnded(ctx context.Context, tx *sql.Tx, task, agent, run string, activity api.AgentActivity) error {
	var ended *api.MatrixWait
	var payload string
	if err := tx.QueryRowContext(ctx, matrixRunEndedPrevious, agent, run).Scan(&payload); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previous := stallActivity(payload, false); previous != nil && previous.MatrixWait.Valid() {
		ended = previous.MatrixWait
	}
	if activity.MatrixWait.Valid() && (ended == nil || !matrixRunReplaced(ended, activity.MatrixWait)) {
		return nil
	}
	to, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=? AND run_id=?`, agent, run))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// A retired agent is kept, not woken.
	if to.Status == api.AgentRetired {
		return nil
	}
	project, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE tasks.id=?`, task))
	if err != nil {
		return err
	}
	endedAt := activity.ObservedAt.UTC().Format(time.RFC3339)
	refs := map[string]string{"action": matrixRunEndedAction, "agent": agent, "run": run, "endedAt": endedAt}
	// With no stored wait to read, the notice names the bound item and no
	// start time.
	item, place, body := "", "", matrixRunEndedRunning
	if ended != nil {
		item = ended.Item
		since := ended.Since.UTC().Format(time.RFC3339)
		refs["matrixRole"], refs["matrixSince"] = ended.Role, since
		place = ", on the host since " + since + ","
		if ended.Role == api.MatrixWaitWaiting {
			place, body = ", waiting for the host since "+since+",", matrixRunEndedWaiting
		}
	}
	if item == "" {
		if err := tx.QueryRowContext(ctx, `SELECT item_id FROM agent_work_item_bindings WHERE agent_id=? AND run_id=?`, agent, run).Scan(&item); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	text := "Your verification run"
	if item != "" {
		refs["item"] = item
		text += " for " + item
	}
	text += place + " " + fmt.Sprintf(body, endedAt) + matrixRunEndedClose
	return s.postBrokerNotice(ctx, tx, project, to, to.Name, matrixRunEndedSubject, matrixRunEndedSubject, text, refs)
}

// stallAgent is one watched run: the helper, or a team member.
type stallAgent struct {
	id, name, host, runtime, runID, status string
	lastSeen, lastWorking, bound           time.Time // bound: when this run was bound or registered
	state                                  string    // stored activity state
	activity                               *api.AgentActivity
	decisions                              int // its unanswered owner decisions
	// lifted is when a never-quiet case of this run last ended: the owner
	// answered its decision request, or its matrix wait ended.
	lifted time.Time
}

func (a stallAgent) online(now time.Time) bool {
	return agentOnlineAt(api.Agent{Status: a.status, LastSeenAt: a.lastSeen}, now)
}

// effective is the activity state at now: a stale row of an agent that
// stopped heartbeating is offline, and no row is unknown.
func (a stallAgent) effective(now time.Time) string {
	switch {
	case !a.online(now):
		return "offline"
	case a.state == "":
		return "unknown"
	}
	return a.state
}

// named is the state a notice names: the roster's needs_input when the
// activity says nothing more.
func (a stallAgent) named(now time.Time) string {
	state := a.effective(now)
	if state == "unknown" && a.status == api.AgentNeedsInput {
		return api.AgentNeedsInput
	}
	return state
}

// workedAt is when the run last worked. An offline agent whose stored state
// is still working worked until its last heartbeat.
func (a stallAgent) workedAt(now time.Time) time.Time {
	if a.state == "working" && !a.online(now) && a.lastSeen.After(a.lastWorking) {
		return a.lastSeen
	}
	return a.lastWorking
}

func (a stallAgent) matrixWait(now time.Time) bool {
	return a.activity != nil && a.activity.MatrixWait.Valid() && now.Sub(a.activity.MatrixWait.Since) < defaultQueueMatrixWaitBound
}

// liftedAt is when the run's last never-quiet case ended. A reported matrix
// wait that outlived its bound ended at the bound.
func (a stallAgent) liftedAt(now time.Time) time.Time {
	if a.activity != nil && a.activity.MatrixWait.Valid() && !a.matrixWait(now) {
		return latest(a.lifted, a.activity.MatrixWait.Since.Add(defaultQueueMatrixWaitBound))
	}
	return a.lifted
}

type stallHelper struct {
	stallAgent
	owed      int
	owedSince time.Time
	oldestSeq int64
}

// stallLifts is when the project's or an entry's planned waits last ended.
// Time spent in a never-quiet case is not quiet time, so each is a floor of
// quiet-since.
type stallLifts struct{ resumed, released time.Time }

type stallTeam struct {
	entry, item   string
	admission     time.Time
	waiting, held bool
	lifts         stallLifts // project resume; hold released or shared-path wait closed
	members       []stallAgent
}

type stallEpisode struct {
	id, kind, subject, runID, recipient, ownerState string
	quietSince, openedAt                            time.Time
	noticeSeq, scanned                              int64
}

// stallView is everything the watch reads about one project.
type stallView struct {
	task     string
	active   bool      // open and not paused
	resumed  time.Time // the project's last resume from a pause
	started  time.Time // the hub process start, when the hub recorded one
	helper   *stallHelper
	teams    []*stallTeam
	episodes []stallEpisode
}

func stallActivity(payload string, full bool) *api.AgentActivity {
	if payload == "" || (!full && !strings.Contains(payload, `"matrixWait"`)) {
		return nil
	}
	var a api.AgentActivity
	if json.Unmarshal([]byte(payload), &a) != nil {
		return nil
	}
	return &a
}

// The times below are compared as text, which orders RFC 3339 times to the
// second; that is exact enough for thresholds of minutes.
const stallAnsweredSQL = `(SELECT COALESCE(MAX(d.created_at),'') FROM decision_requests r JOIN messages m ON m.seq=r.message_seq
 JOIN decision_answers d ON d.task_id=r.task_id AND d.request_seq=r.message_seq
 WHERE r.task_id=a.task_id AND m.from_agent=a.id AND m.from_run_id=a.run_id)`

const stallResumedSQL = `(SELECT COALESCE(MAX(c.resumed_at),'') FROM project_pause_cycles c WHERE c.task_id=t.id)`

const stallDecisionsSQL = `(SELECT count(*) FROM decision_requests r JOIN messages m ON m.seq=r.message_seq
 LEFT JOIN decision_answers d ON d.task_id=r.task_id AND d.request_seq=r.message_seq
 WHERE r.task_id=a.task_id AND m.from_agent=a.id AND m.from_run_id=a.run_id AND d.message_seq IS NULL)`

// loadStallViews is the watch's three reads, for one project or, with an
// empty task, for all of them. Each result set is read to its end before the
// next query, as the hub's single connection requires.
func loadStallViews(ctx context.Context, q queryRower, task string) (map[string]*stallView, error) {
	views := map[string]*stallView{}
	view := func(id string) *stallView {
		v := views[id]
		if v == nil {
			v = &stallView{task: id}
			views[id] = v
		}
		return v
	}
	// 1. Open episodes, through the partial index.
	rows, err := q.QueryContext(ctx, `SELECT e.id,e.task_id,e.kind,e.subject_id,e.run_id,e.recipient,e.owner_state,e.quiet_since,e.opened_at,e.notice_seq,e.scanned_seq,
 t.status=? AND t.pause_state=? FROM stall_watch_episodes e JOIN tasks t ON t.id=e.task_id
 WHERE e.closed_at='' AND (?='' OR e.task_id=?) ORDER BY e.opened_at,e.id`, api.TaskOpen, api.ProjectPauseActive, task, task)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e stallEpisode
		var id, quiet, opened string
		var active bool
		if err := rows.Scan(&e.id, &id, &e.kind, &e.subject, &e.runID, &e.recipient, &e.ownerState, &quiet, &opened, &e.noticeSeq, &e.scanned, &active); err != nil {
			rows.Close()
			return nil, err
		}
		e.quietSince, e.openedAt = parseTS(quiet), parseTS(opened)
		v := view(id)
		v.active = active
		v.episodes = append(v.episodes, e)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// 2. Live helpers of open, active projects, one row per open obligation
	// that is not delivery-only. Who sent it is judged below.
	rows, err = q.QueryContext(ctx, `SELECT a.task_id,a.id,a.name,a.host,a.runtime,a.run_id,a.status,COALESCE(a.last_seen_at,''),
 COALESCE(x.state,''),COALESCE(x.payload,''),COALESCE(p.last_working_at,''),`+stallDecisionsSQL+`,
 COALESCE(p.wait_ended_at,''),`+stallAnsweredSQL+`,`+stallResumedSQL+`,
 COALESCE((SELECT g.created_at FROM owner_helper_registrations g WHERE g.task_id=a.task_id AND g.agent_id=a.id AND g.run_id=a.run_id),''),
 COALESCE(o.message_seq,0),COALESCE(o.created_at,''),COALESCE(m.from_agent,''),COALESCE(m.from_node,'')
 FROM agents a JOIN tasks t ON t.id=a.task_id
 LEFT JOIN agent_activity x ON x.agent_id=a.id AND x.run_id=a.run_id
 LEFT JOIN stall_watch_progress p ON p.agent_id=a.id AND p.run_id=a.run_id
 LEFT JOIN obligations o ON o.agent_id=a.id AND o.task_id=a.task_id AND o.state<>? AND o.needs<>?
 LEFT JOIN messages m ON m.seq=o.message_seq
 WHERE a.role=? AND a.status NOT IN ('closed','exited','retired') AND t.status=? AND t.pause_state=? AND (?='' OR a.task_id=?)
 ORDER BY a.task_id,a.created_at DESC,a.id,o.message_seq`,
		api.ObligationClosed, api.ObligationNeedsDelivery, api.AgentRoleOwnerHelper, api.TaskOpen, api.ProjectPauseActive, task, task)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a stallAgent
		var id, seen, payload, worked, waited, answered, resumed, registered, owedAt, fromAgent, fromNode string
		var seq int64
		if err := rows.Scan(&id, &a.id, &a.name, &a.host, &a.runtime, &a.runID, &a.status, &seen, &a.state, &payload, &worked, &a.decisions, &waited, &answered, &resumed, &registered, &seq, &owedAt, &fromAgent, &fromNode); err != nil {
			rows.Close()
			return nil, err
		}
		v := view(id)
		v.active, v.resumed = true, parseTS(resumed)
		if v.helper == nil {
			a.lifted = latest(parseTS(waited), parseTS(answered))
			a.lastSeen, a.lastWorking, a.bound, a.activity = parseTS(seen), parseTS(worked), parseTS(registered), stallActivity(payload, true)
			v.helper = &stallHelper{stallAgent: a}
		}
		h := v.helper
		// Owed to the owner: a directed message or typed request from a
		// person. Hub notices and teammates' requests do not count.
		if h.id != a.id || seq == 0 || fromAgent != "" || fromNode == api.BrokerNode {
			continue
		}
		h.owed++
		if at := parseTS(owedAt); h.owedSince.IsZero() || at.Before(h.owedSince) {
			h.owedSince, h.oldestSeq = at, seq
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// 3. Running entries of open, active projects with the live runs bound to
	// their items. Membership by admission is judged below.
	rows, err = q.QueryContext(ctx, `SELECT e.task_id,e.id,e.item_id,e.created_at,e.admitted_at,
 EXISTS(SELECT 1 FROM team_queue_waits w WHERE w.task_id=e.task_id AND w.entry_id=e.id AND w.state IN ('waiting','met','orphaned')),
 EXISTS(SELECT 1 FROM team_queue_budget_holds h WHERE h.task_id=e.task_id AND h.entry_id=e.id AND h.state='held'),
 COALESCE(a.id,''),COALESCE(a.name,''),COALESCE(a.run_id,''),COALESCE(a.status,''),COALESCE(a.last_seen_at,''),COALESCE(b.created_at,''),
 COALESCE(x.state,''),COALESCE(x.payload,''),COALESCE(p.last_working_at,''),CASE WHEN a.id IS NULL THEN 0 ELSE `+stallDecisionsSQL+` END,
 COALESCE(p.wait_ended_at,''),CASE WHEN a.id IS NULL THEN '' ELSE `+stallAnsweredSQL+` END,`+stallResumedSQL+`,
 (SELECT COALESCE(MAX(h.resolved_at),'') FROM team_queue_budget_holds h WHERE h.task_id=e.task_id AND h.entry_id=e.id AND h.state<>'held'),
 (SELECT COALESCE(MAX(w.closed_at),'') FROM team_queue_waits w WHERE w.task_id=e.task_id AND w.entry_id=e.id AND w.state NOT IN ('waiting','met','orphaned'))
 FROM team_queue_entries e JOIN tasks t ON t.id=e.task_id
 LEFT JOIN agent_work_item_bindings b ON b.item_task_id=e.task_id AND b.item_id=e.item_id
 LEFT JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id AND a.role='' AND a.status NOT IN ('closed','exited','retired')
 LEFT JOIN agent_activity x ON x.agent_id=a.id AND x.run_id=a.run_id
 LEFT JOIN stall_watch_progress p ON p.agent_id=a.id AND p.run_id=a.run_id
 WHERE e.state='running' AND e.released_at='' AND t.status=? AND t.pause_state=? AND (?='' OR e.task_id=?)
 ORDER BY e.task_id,e.position,e.id,a.name,a.id`, api.TaskOpen, api.ProjectPauseActive, task, task)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a stallAgent
		var id, entry, item, created, admitted, seen, bound, payload, worked, waited, answered, resumed, released, cleared string
		var waiting, held bool
		if err := rows.Scan(&id, &entry, &item, &created, &admitted, &waiting, &held, &a.id, &a.name, &a.runID, &a.status, &seen, &bound, &a.state, &payload, &worked, &a.decisions,
			&waited, &answered, &resumed, &released, &cleared); err != nil {
			rows.Close()
			return nil, err
		}
		v := view(id)
		v.active, v.resumed = true, parseTS(resumed)
		var team *stallTeam
		if n := len(v.teams); n > 0 && v.teams[n-1].entry == entry {
			team = v.teams[n-1]
		} else {
			team = &stallTeam{entry: entry, item: item, admission: parseTS(admitted), waiting: waiting, held: held,
				lifts: stallLifts{resumed: v.resumed, released: latest(parseTS(released), parseTS(cleared))}}
			if team.admission.IsZero() {
				team.admission = parseTS(created)
			}
			v.teams = append(v.teams, team)
		}
		// A run bound before this admission belongs to an earlier team.
		a.bound = parseTS(bound)
		if a.id == "" || a.bound.IsZero() || a.bound.Before(team.admission) {
			continue
		}
		a.lastSeen, a.lastWorking, a.activity = parseTS(seen), parseTS(worked), stallActivity(payload, false)
		a.lifted = latest(parseTS(waited), parseTS(answered))
		team.members = append(team.members, a)
	}
	return views, rows.Close()
}

type stallRun struct{ agent, run string }

type stallPost struct {
	seq int64
	stallRun
}

// stallWatchRecentPosts reads the project's messages newest first and stops
// at the first one older than since. It returns each exact run's newest post
// and the number of rows read.
func stallWatchRecentPosts(ctx context.Context, q queryRower, task string, since time.Time) (map[stallRun]time.Time, int, error) {
	rows, err := q.QueryContext(ctx, `SELECT from_agent,COALESCE(from_run_id,''),created_at FROM messages WHERE task_id=? ORDER BY seq DESC`, task)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	posts := map[stallRun]time.Time{}
	read := 0
	for rows.Next() {
		var r stallRun
		var created string
		if err := rows.Scan(&r.agent, &r.run, &created); err != nil {
			return nil, read, err
		}
		read++
		at := parseTS(created)
		if at.Before(since) {
			break
		}
		if r.agent != "" && at.After(posts[r]) {
			posts[r] = at
		}
	}
	if err := rows.Err(); err != nil {
		return nil, read, err
	}
	return posts, read, rows.Close()
}

// stallWatchEpisodePosts reads at most stallWatchScanLimit messages after a
// cursor, oldest first, and returns the agent posts among them, the last
// message number read and the number of rows read.
func stallWatchEpisodePosts(ctx context.Context, q queryRower, task string, after int64) ([]stallPost, int64, int, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq,from_agent,COALESCE(from_run_id,'') FROM messages WHERE task_id=? AND seq>? ORDER BY seq LIMIT ?`, task, after, stallWatchScanLimit)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	var posts []stallPost
	var last int64
	read := 0
	for rows.Next() {
		var p stallPost
		if err := rows.Scan(&p.seq, &p.agent, &p.run); err != nil {
			return nil, last, read, err
		}
		read++
		last = p.seq
		if p.agent != "" {
			posts = append(posts, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, last, read, err
	}
	return posts, last, read, rows.Close()
}

// stallReads holds one project's two bounded message reads, each made at
// most once per decision and only when a subject needs it.
type stallReads struct {
	ctx  context.Context
	q    queryRower
	task string
	now  time.Time
	from int64 // the smallest cursor among the project's open episodes

	recent     map[stallRun]time.Time
	recentDone bool
	later      []stallPost
	laterLast  int64
	laterRead  int
	laterDone  bool
}

func (r *stallReads) recentPost(run stallRun) (time.Time, error) {
	if !r.recentDone {
		posts, _, err := stallWatchRecentPosts(r.ctx, r.q, r.task, r.now.Add(-stallWatchPostWindow))
		if err != nil {
			return time.Time{}, err
		}
		r.recent, r.recentDone = posts, true
	}
	return r.recent[run], nil
}

// postedAfter reports whether one of runs posted after seq, among the
// messages read from the project's cursor.
func (r *stallReads) postedAfter(seq int64, runs map[stallRun]bool) (bool, error) {
	if !r.laterDone {
		posts, last, read, err := stallWatchEpisodePosts(r.ctx, r.q, r.task, r.from)
		if err != nil {
			return false, err
		}
		r.later, r.laterLast, r.laterRead, r.laterDone = posts, last, read, true
	}
	for _, p := range r.later {
		if p.seq > seq && runs[p.stallRun] {
			return true, nil
		}
	}
	return false, nil
}

// stallNotice is one notice the write phase posts for an episode.
type stallNotice struct {
	action, subject, text string
	toHelper              bool
	refs                  map[string]string
}

type stallOpen struct {
	id, kind, subject, runID, item, activity string
	quietSince                               time.Time
	recipient, ownerState                    string
	notice                                   stallNotice
}

type stallPing struct {
	episode string
	notice  stallNotice
}

type stallClose struct{ episode, reason string }

// stallPlan is what one project's decision calls for.
type stallPlan struct {
	closes  []stallClose
	opens   []stallOpen
	pings   []stallPing
	advance map[string]int64 // episode cursors to move forward
}

func (p stallPlan) empty() bool {
	return len(p.closes) == 0 && len(p.opens) == 0 && len(p.pings) == 0 && len(p.advance) == 0
}

func latest(times ...time.Time) time.Time {
	var out time.Time
	for _, t := range times {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// unheard reports whether the hub started less than the hold ago and has not
// heard from the agent since. Such an agent is offline only as far as the hub
// knows, so it is not judged.
func (v *stallView) unheard(a stallAgent, now time.Time) bool {
	return !v.started.IsZero() && !now.Before(v.started) && now.Sub(v.started) < stallWatchStartHold && a.lastSeen.Before(v.started)
}

// helperNeverQuiet names the case that keeps the helper from being quiet, or
// is empty: it works, waits for the matrix host, or waits for the owner's
// answer to its own decision request.
func (v *stallView) helperNeverQuiet(now time.Time) string {
	h := v.helper
	switch {
	case h.effective(now) == "working":
		return "working"
	case h.matrixWait(now):
		return "matrix-wait"
	case h.decisions > 0:
		return "owner-decision"
	}
	return ""
}

// helperQuiet is the helper definition: it owes the owner, no never-quiet
// case holds, and neither a post, a working report, a newer owed item, the
// registration of its current run nor the end of a never-quiet case falls
// inside the threshold.
func (v *stallView) helperQuiet(now time.Time, reads *stallReads) (time.Time, bool, error) {
	h := v.helper
	if h == nil || h.owed == 0 || v.helperNeverQuiet(now) != "" {
		return time.Time{}, false, nil
	}
	since := latest(h.owedSince, h.workedAt(now), h.bound, h.liftedAt(now), v.resumed)
	if now.Sub(since) < stallWatchHelperQuiet {
		return since, false, nil
	}
	posted, err := reads.recentPost(stallRun{h.id, h.runID})
	if err != nil {
		return since, false, err
	}
	since = latest(since, posted)
	return since, now.Sub(since) >= stallWatchHelperQuiet, nil
}

// teamNeverQuiet names the planned wait or the progress that keeps a team
// from being quiet, or is empty.
func (t *stallTeam) neverQuiet(now time.Time) string {
	switch {
	case t.waiting:
		return "shared-path-wait"
	case t.held:
		return "held"
	}
	for _, m := range t.members {
		if m.decisions > 0 {
			return "owner-decision"
		}
	}
	for _, m := range t.members {
		if m.effective(now) == "working" {
			return "working"
		}
	}
	for _, m := range t.members {
		if m.matrixWait(now) {
			return "matrix-wait"
		}
	}
	return ""
}

func (t *stallTeam) runs() map[stallRun]bool {
	runs := map[stallRun]bool{}
	for _, m := range t.members {
		runs[stallRun{m.id, m.runID}] = true
	}
	return runs
}

// decide computes the project's whole decision from the view. The unlocked
// read uses it to nominate a project and the write transaction uses it again
// on a fresh view, so nothing is posted on a stale read.
func (v *stallView) decide(ctx context.Context, q queryRower, now time.Time) (stallPlan, error) {
	plan := stallPlan{advance: map[string]int64{}}
	reads := &stallReads{ctx: ctx, q: q, task: v.task, now: now}
	for i, e := range v.episodes {
		if i == 0 || e.scanned < reads.from {
			reads.from = e.scanned
		}
	}
	night := stallWatchNight(now)
	open := map[string]stallEpisode{} // episodes that still hold, by kind and subject
	teams := map[string]*stallTeam{}
	for _, t := range v.teams {
		teams[t.entry] = t
	}
	for _, e := range v.episodes {
		reason, err := v.ended(e, teams[e.subject], now, reads)
		if err != nil {
			return plan, err
		}
		if reason != "" {
			plan.closes = append(plan.closes, stallClose{e.id, reason})
			continue
		}
		open[e.kind+":"+e.subject] = e
	}
	// Closing is decided first, so an episode that ended in the night, or
	// whose subject is now in a never-quiet case, never pings.
	helperSince, helperQuiet, err := v.helperQuiet(now, reads)
	if err != nil {
		return plan, err
	}
	// An unheard helper is neither opened nor pinged; closing above is not held.
	helperUnheard := v.helper != nil && v.unheard(v.helper.stallAgent, now)
	if h := v.helper; h != nil && !helperUnheard {
		e, isOpen := open[stallKindHelper+":"+h.id]
		switch {
		case isOpen && e.ownerState == stallOwnerDeferred && !night:
			plan.pings = append(plan.pings, stallPing{e.id, v.helperNotice(e.id, e.quietSince, now, false, e.noticeSeq)})
		case !isOpen && helperQuiet:
			id := api.NewID("swe")
			o := stallOpen{id: id, kind: stallKindHelper, subject: h.id, runID: h.runID, activity: h.named(now), quietSince: helperSince,
				recipient: stallRecipientOwner, ownerState: stallOwnerPinged, notice: v.helperNotice(id, helperSince, now, night, 0)}
			if night {
				o.ownerState = stallOwnerDeferred
			}
			plan.opens = append(plan.opens, o)
		}
	}
	// Who hears a team episode, decided when it opens: the helper when one is
	// live, online, not itself quiet and not crashed, stuck or blocked by its
	// provider; otherwise the owner.
	helperState := "not registered"
	if h := v.helper; h != nil {
		switch {
		case !h.online(now):
			helperState = "offline"
		case helperQuiet:
			helperState = "quiet too"
		case stallHelperDown[h.effective(now)] != "":
			helperState = stallHelperDown[h.effective(now)]
		default:
			helperState = ""
		}
	}
	for _, t := range v.teams {
		// After a hub start no team is opened or pinged while the helper or
		// one of its members is unheard.
		held := helperUnheard
		for _, m := range t.members {
			held = held || v.unheard(m, now)
		}
		if held {
			continue
		}
		e, isOpen := open[stallKindTeam+":"+t.entry]
		if isOpen {
			if e.ownerState == stallOwnerDeferred && !night {
				// The recipient was chosen when the episode opened.
				why := helperState
				if why == "" {
					why = "available again, but was not when this episode opened"
				}
				plan.pings = append(plan.pings, stallPing{e.id, t.notice(e.id, e.quietSince, now, why, false, true, e.noticeSeq)})
			}
			continue
		}
		// A team with no member is left to the queue's nothing-running stall.
		if len(t.members) == 0 || t.neverQuiet(now) != "" {
			continue
		}
		// The end of a planned wait starts the quiet time afresh.
		since := latest(t.admission, t.lifts.resumed, t.lifts.released)
		for _, m := range t.members {
			since = latest(since, m.workedAt(now), m.bound, m.liftedAt(now))
		}
		if now.Sub(since) < stallWatchTeamQuiet {
			continue
		}
		for _, m := range t.members {
			posted, err := reads.recentPost(stallRun{m.id, m.runID})
			if err != nil {
				return plan, err
			}
			since = latest(since, posted)
		}
		if now.Sub(since) < stallWatchTeamQuiet {
			continue
		}
		id := api.NewID("swe")
		o := stallOpen{id: id, kind: stallKindTeam, subject: t.entry, item: t.item, quietSince: since, recipient: stallRecipientHelper}
		switch {
		case helperState == "":
			o.notice = t.notice(id, since, now, "", false, false, 0)
		case night:
			o.recipient, o.ownerState = stallRecipientOwner, stallOwnerDeferred
			o.notice = t.notice(id, since, now, helperState, true, true, 0)
		default:
			o.recipient, o.ownerState = stallRecipientOwner, stallOwnerPinged
			o.notice = t.notice(id, since, now, helperState, false, true, 0)
		}
		plan.opens = append(plan.opens, o)
	}
	// An episode read that filled its limit without an ending post moves the
	// cursors, so the next sweep continues from there.
	if reads.laterDone && reads.laterRead >= stallWatchScanLimit {
		for _, e := range open {
			if e.scanned < reads.laterLast {
				plan.advance[e.id] = reads.laterLast
			}
		}
	}
	return plan, nil
}

// ended names why an open episode no longer holds, or is empty. An ending
// post is looked for last, in the messages after the episode's own cursor.
func (v *stallView) ended(e stallEpisode, team *stallTeam, now time.Time, reads *stallReads) (string, error) {
	if !v.active {
		return "paused", nil
	}
	if e.kind == stallKindHelper {
		h := v.helper
		switch {
		case h == nil || h.id != e.subject:
			return "helper-gone", nil
		case h.runID != e.runID:
			return "run-replaced", nil
		case h.owed == 0:
			return "nothing-owed", nil
		case h.workedAt(now).After(e.openedAt):
			return "working", nil
		}
		if reason := v.helperNeverQuiet(now); reason != "" {
			return reason, nil
		}
		posted, err := reads.postedAfter(e.scanned, map[stallRun]bool{{h.id, h.runID}: true})
		if err != nil || !posted {
			return "", err
		}
		return "posted", nil
	}
	if team == nil {
		return "entry-not-running", nil
	}
	if reason := team.neverQuiet(now); reason != "" {
		return reason, nil
	}
	if len(team.members) == 0 {
		return "no-members", nil
	}
	for _, m := range team.members {
		if m.workedAt(now).After(e.openedAt) {
			return "working", nil
		}
	}
	posted, err := reads.postedAfter(e.scanned, team.runs())
	if err != nil || !posted {
		return "", err
	}
	return "posted", nil
}

func stallDuration(d time.Duration) string {
	minutes := int(d / time.Minute)
	if minutes < 60 {
		return fmt.Sprintf("%d min", minutes)
	}
	return fmt.Sprintf("%d h %d min", minutes/60, minutes%60)
}

// stallHelperDown names a helper that heartbeats but cannot act on a notice.
var stallHelperDown = map[string]string{
	"crashed": "crashed", "stuck": "stuck", "provider_blocked": "blocked by its provider",
}

// stallHelperNextStep is the usual step for a helper state. It is offered
// only after the record of why the helper stopped, never as the cause.
func stallHelperNextStep(state, runtime, class, host string) string {
	switch state {
	case "idle", "finished_silent":
		return "look at the helper's session; if its last turn ended normally, prompt it"
	case "stuck", "hung_tool", "looping":
		return "look at what the session is waiting on before interrupting it"
	case "crashed":
		return "check the session's last output; a restart needs `tt helper register` afterwards"
	case "provider_blocked":
		return providerBlockRecovery(runtime, class, host)
	case "runtime_prompt":
		return "read the prompt showing in the helper's terminal"
	case "offline":
		return "check the relay on the host with `tt doctor` and whether the session is still open"
	}
	return "open the helper's session and see what it shows"
}

// stallHelperText is the helper notice: what was observed, that the cause is
// not established, the record owed before resuming, then one suggestion.
func stallHelperText(name, host, state, status string, quietSince, now time.Time, owed int, oldestSeq int64, nextStep string) string {
	return fmt.Sprintf("%s on %s has been quiet for %s: no post and no working report since %s. Its activity state is %s (roster status %s). It owes the owner %d open item(s); the oldest is message #%d. %s %s %s%s.",
		name, host, stallDuration(now.Sub(quietSince)), quietSince.UTC().Format(time.RFC3339), state, status, owed, oldestSeq, stallUnknownCause, stallRecordFirst, stallNextStep, nextStep)
}

func stallNoticeRefs(kind, episode string, owner, night bool) map[string]string {
	refs := map[string]string{"stallWatch": kind, "episode": episode}
	if owner {
		refs["recipientKind"], refs["escalation"] = "owner", "owner"
		// At night the notice is a channel line: the bridge mirrors a
		// lead-level escalation without a mention.
		if night {
			refs["escalation"] = "lead"
		}
	}
	return refs
}

// helperNotice builds the helper watch's notice. deferredFrom is the night
// line's message number when this is the 07:00 ping.
func (v *stallView) helperNotice(episode string, since, now time.Time, night bool, deferredFrom int64) stallNotice {
	h := v.helper
	state := h.named(now)
	class := ""
	if h.activity != nil && h.activity.Provider != nil {
		class = h.activity.Provider.Class
	}
	text := stallHelperText(h.name, h.host, state, h.status, since, now, h.owed, h.oldestSeq, stallHelperNextStep(state, h.runtime, class, h.host))
	n := stallNotice{action: stallActionHelper, subject: stallHelperSubject, refs: stallNoticeRefs(stallKindHelper, episode, true, night)}
	n.refs["agent"], n.refs["run"], n.refs["activity"] = h.id, h.runID, state
	switch {
	case deferredFrom > 0:
		n.action = stallActionHelperPing
		text = fmt.Sprintf("Still open since the night line #%d. %s", deferredFrom, text)
	case night:
		text += stallNightLine
	}
	n.text = text
	return n
}

// notice builds a team watch notice. helperState says why the owner hears it
// instead of the helper; it is empty for the directed helper notice.
func (t *stallTeam) notice(episode string, since, now time.Time, helperState string, night, owner bool, deferredFrom int64) stallNotice {
	var members []string
	for _, m := range t.members {
		members = append(members, fmt.Sprintf("%s (%s)", m.name, m.named(now)))
	}
	sort.Strings(members)
	text := fmt.Sprintf("The team of queue entry %s (item %s) has been quiet for %s: no post, no working report and no matrix wait from its %d member(s) since %s. Members: %s.",
		t.entry, t.item, stallDuration(now.Sub(since)), len(t.members), since.UTC().Format(time.RFC3339), providerNameList(members, 600))
	if helperState != "" {
		text += fmt.Sprintf(" The owner helper is %s, so this goes to the owner.", helperState)
	}
	text += fmt.Sprintf(" %s %s %sread the team's last messages and look at its lead's session, then decide whether to prompt the lead.", stallUnknownCause, stallRecordFirst, stallNextStep)
	n := stallNotice{action: stallActionTeam, subject: stallTeamSubject, toHelper: !owner, refs: stallNoticeRefs(stallKindTeam, episode, owner, night)}
	n.refs["entry"], n.refs["item"] = t.entry, t.item
	if owner {
		n.subject = stallTeamOwnerSubject
	}
	switch {
	case deferredFrom > 0:
		n.action = stallActionTeamPing
		text = fmt.Sprintf("Still open since the night line #%d. %s", deferredFrom, text)
	case night:
		text += stallNightLine
	}
	n.text = text
	return n
}

// StallWatchSweep opens, pings and closes the stall watch's episodes for
// every open, active project and returns one step per notice it posted. With
// nothing quiet and no open episode it makes three reads and no write. It
// decides from the given now and reads only durable rows.
func (s *Store) StallWatchSweep(ctx context.Context, now time.Time) ([]StallWatchStep, error) {
	views, err := loadStallViews(ctx, s.db, "")
	if err != nil {
		return nil, fmt.Errorf("stall watch sweep: %w", err)
	}
	s.stallWatchStarted(views)
	var tasks []string
	for id := range views {
		tasks = append(tasks, id)
	}
	sort.Strings(tasks)
	var firstErr error
	note := func(task string, err error) {
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("stall watch sweep %s: %w", task, err)
		}
	}
	// The unlocked read only nominates projects.
	var nominated []string
	for _, task := range tasks {
		plan, err := views[task].decide(ctx, s.db, now)
		note(task, err)
		if err == nil && !plan.empty() {
			nominated = append(nominated, task)
		}
	}
	if stallWatchBeforeWrite != nil {
		stallWatchBeforeWrite()
	}
	var steps []StallWatchStep
	for _, task := range nominated {
		posted, err := s.stallWatchWrite(ctx, task, now)
		steps = append(steps, posted...)
		note(task, err)
	}
	return steps, firstErr
}

// stallWatchWrite decides one nominated project again inside the write
// transaction and applies what that decision calls for: an episode row and
// its notice commit together.
func (s *Store) stallWatchWrite(ctx context.Context, task string, now time.Time) ([]StallWatchStep, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	views, err := loadStallViews(ctx, tx, task)
	if err != nil {
		return nil, err
	}
	s.stallWatchStarted(views)
	v := views[task]
	if v == nil {
		return nil, nil
	}
	plan, err := v.decide(ctx, tx, now)
	if err != nil || plan.empty() {
		return nil, err
	}
	for _, c := range plan.closes {
		if _, err := tx.ExecContext(ctx, `UPDATE stall_watch_episodes SET closed_at=?,close_reason=? WHERE id=? AND closed_at=''`, ts(now), c.reason, c.episode); err != nil {
			return nil, err
		}
	}
	for id, seq := range plan.advance {
		if _, err := tx.ExecContext(ctx, `UPDATE stall_watch_episodes SET scanned_seq=? WHERE id=? AND scanned_seq<?`, seq, id, seq); err != nil {
			return nil, err
		}
	}
	var steps []StallWatchStep
	if len(plan.opens)+len(plan.pings) > 0 {
		project, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE tasks.id=?`, task))
		if err != nil {
			return nil, err
		}
		post := func(n stallNotice) (int64, error) {
			var to api.Agent
			name := ""
			if n.toHelper {
				if to, err = scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, v.helper.id)); err != nil {
					return 0, err
				}
				name = to.Name
			}
			if err := s.postBrokerNotice(ctx, tx, project, to, name, n.subject, n.subject, n.text, n.refs); err != nil {
				return 0, err
			}
			var seq int64
			if err := tx.QueryRowContext(ctx, `SELECT max(seq) FROM messages WHERE task_id=?`, task).Scan(&seq); err != nil {
				return 0, err
			}
			steps = append(steps, StallWatchStep{TaskID: task, MessageSeq: seq, Action: n.action})
			return seq, nil
		}
		for _, p := range plan.pings {
			seq, err := post(p.notice)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE stall_watch_episodes SET owner_state=?,ping_seq=? WHERE id=?`, stallOwnerPinged, seq, p.episode); err != nil {
				return nil, err
			}
		}
		for _, o := range plan.opens {
			seq, err := post(o.notice)
			if err != nil {
				return nil, err
			}
			// The partial unique index refuses a second open episode for the
			// subject, which rolls its notice back too.
			if _, err := tx.ExecContext(ctx, `INSERT INTO stall_watch_episodes(id,task_id,kind,subject_id,run_id,item_id,activity,quiet_since,opened_at,recipient,owner_state,notice_seq,scanned_seq) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				o.id, task, o.kind, o.subject, o.runID, o.item, o.activity, ts(o.quietSince), ts(now), o.recipient, o.ownerState, seq, seq); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if len(steps) > 0 {
		s.notify(task)
	}
	return steps, nil
}
