package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Agent rows change from many tables and from computed fields (activity,
// bindings, rotations, read cursors, unread counts, online by time), and some
// of those writes emit no feed event. So the agents read derives its changes:
// it keeps, per project, a digest and a version of each agent it last
// returned, and a client that sends back the cursor of its last read gets only
// the agents whose JSON changed since, plus the ids that left.
//
// The versions live in this process only. A random epoch in the cursor makes a
// cursor from an earlier hub process reset to the whole list.

// rosterTombstones bounds the removed ids kept per project. Dropping older
// ones raises the floor below which a cursor resets.
const rosterTombstones = 1024

type rosterTracker struct {
	epoch string
	mu    sync.Mutex
	tasks map[string]*taskRoster
}

type rosterEntry struct {
	digest  [sha256.Size]byte
	version int64
}

type taskRoster struct {
	// mu is held across the store read and the diff, so an older snapshot is
	// never versioned after a newer one.
	mu      sync.Mutex
	version int64
	agents  map[string]rosterEntry
	removed map[string]int64
	floor   int64
}

// rosterDelta is one agents read: the whole roster, the agents changed after
// the cursor, and the ids removed after it.
type rosterDelta struct {
	All     []api.Agent
	Changed []api.Agent
	Removed []string
	Cursor  string
	// Reset means the cursor could not be served: the caller gets All.
	Reset bool
}

func newRosterTracker() *rosterTracker {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return &rosterTracker{epoch: hex.EncodeToString(b), tasks: map[string]*taskRoster{}}
}

func (t *rosterTracker) roster(task string) *taskRoster {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.tasks[task]
	if r == nil {
		r = &taskRoster{agents: map[string]rosterEntry{}, removed: map[string]int64{}}
		t.tasks[task] = r
	}
	return r
}

// parseRosterCursor reads "EPOCH.N". ok is false for a malformed cursor.
func parseRosterCursor(value string) (epoch string, n int64, ok bool) {
	epoch, raw, found := strings.Cut(value, ".")
	if !found || epoch == "" || strings.ContainsAny(epoch, ".") {
		return "", 0, false
	}
	if _, err := hex.DecodeString(epoch); err != nil {
		return "", 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != raw {
		return "", 0, false
	}
	return epoch, n, true
}

// read lists the project's agents, versions what changed since the previous
// read, and answers relative to since ("" for a whole-list read).
func (t *rosterTracker) read(ctx context.Context, task, since string, list func(context.Context, string) ([]api.Agent, error)) (rosterDelta, error) {
	r := t.roster(task)
	r.mu.Lock()
	defer r.mu.Unlock()
	agents, err := list(ctx, task)
	if err != nil {
		return rosterDelta{}, err
	}
	next := r.version + 1
	bumped := false
	seen := make(map[string]bool, len(agents))
	versions := make([]int64, len(agents))
	for i, agent := range agents {
		b, err := json.Marshal(agent)
		if err != nil {
			return rosterDelta{}, err
		}
		digest := sha256.Sum256(b)
		seen[agent.ID] = true
		entry, ok := r.agents[agent.ID]
		if !ok || entry.digest != digest {
			entry = rosterEntry{digest: digest, version: next}
			r.agents[agent.ID] = entry
			delete(r.removed, agent.ID)
			bumped = true
		}
		versions[i] = entry.version
	}
	for id := range r.agents {
		if !seen[id] {
			delete(r.agents, id)
			r.removed[id] = next
			bumped = true
		}
	}
	if bumped {
		r.version = next
	}
	r.prune()
	delta := rosterDelta{All: agents, Cursor: t.epoch + "." + strconv.FormatInt(r.version, 10)}
	if since == "" {
		return delta, nil
	}
	epoch, n, _ := parseRosterCursor(since)
	if epoch != t.epoch || n > r.version || n < r.floor {
		delta.Reset = true
		return delta, nil
	}
	delta.Changed = []api.Agent{}
	for i, agent := range agents {
		if versions[i] > n {
			delta.Changed = append(delta.Changed, agent)
		}
	}
	delta.Removed = []string{}
	for id, version := range r.removed {
		if version > n {
			delta.Removed = append(delta.Removed, id)
		}
	}
	sort.Strings(delta.Removed)
	return delta, nil
}

// prune keeps at most rosterTombstones removed ids, dropping the oldest and
// raising the floor to the newest version dropped.
func (r *taskRoster) prune() {
	for len(r.removed) > rosterTombstones {
		oldestID, oldest := "", int64(-1)
		for id, version := range r.removed {
			if oldest < 0 || version < oldest || (version == oldest && id < oldestID) {
				oldestID, oldest = id, version
			}
		}
		delete(r.removed, oldestID)
		if oldest > r.floor {
			r.floor = oldest
		}
	}
}

// agentsPage is the agents route's answer. A whole-list read sets Agents and
// Cursor; a since read with Changes sets Agents to the changed agents and
// Removed to the ids that left.
type agentsPage struct {
	Agents  []api.Agent `json:"agents"`
	Removed []string    `json:"removed,omitempty"`
	Cursor  string      `json:"cursor"`
	Changes bool        `json:"changes,omitempty"`
	Reset   bool        `json:"reset,omitempty"`
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	since := q.Get("since")
	if q.Has("since") {
		if _, _, ok := parseRosterCursor(since); !ok {
			writeError(w, http.StatusBadRequest, "since needs a cursor from an agents read")
			return
		}
	}
	if _, err := s.store.GetTask(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	delta, err := s.rosters.read(r.Context(), id, since, s.store.ListAgents)
	if err != nil {
		fail(w, err)
		return
	}
	if since == "" || delta.Reset {
		writeJSON(w, 200, agentsPage{Agents: delta.All, Cursor: delta.Cursor, Reset: delta.Reset})
		return
	}
	writeJSON(w, 200, agentsPage{Agents: delta.Changed, Removed: delta.Removed, Cursor: delta.Cursor, Changes: true})
}
