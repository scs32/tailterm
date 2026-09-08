// Package api defines the JSON shapes shared by the hub server and the tt CLI.
package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxNameLen       = 64
	MaxTextLen       = 8 * 1024
	MaxDataLen       = 4 * 1024
	MaxAgentsPerTask = 32
	MaxTasks         = 200
	MaxEventsPerTask = 10000
	MaxBody          = 64 * 1024
	MaxWait          = 30 * time.Second
	MaxLimit         = 200
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ValidName reports whether s is an acceptable agent or session name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Task titles are display text, independent of tmux session names.
func ValidTaskName(s string) bool {
	if s == "" || strings.TrimSpace(s) != s || utf8.RuneCountInString(s) > 120 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ValidText rejects oversized text and control characters other than tab and newline.
func ValidText(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
			return false
		}
		if r == 0x7f {
			return false
		}
	}
	return true
}

// NewID returns a prefixed random identifier such as tsk_0123456789abcdef.
func NewID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

var idRE = regexp.MustCompile(`^(tsk|agt)_[0-9a-f]{16}$`)

// ValidID reports whether s is a well-formed task or agent id with the given prefix.
func ValidID(s, prefix string) bool {
	return idRE.MatchString(s) && strings.HasPrefix(s, prefix+"_")
}

// Caller identifies who made a request, derived from Tailscale WhoIs.
type Caller struct {
	Node string `json:"node"`
	User string `json:"user"`
}

// Task statuses.
const (
	TaskOpen   = "open"
	TaskClosed = "closed"
)

// Agent statuses.
const (
	AgentStarting   = "starting"
	AgentRunning    = "running"
	AgentDone       = "done"
	AgentRetired    = "retired"
	AgentNeedsInput = "needs_input"
	AgentClosed     = "closed"
	AgentExited     = "exited"
)

// Event kinds.
const (
	EventTaskCreated = "task_created"
	EventTaskClosed  = "task_closed"
	EventAgentAdded  = "agent_added"
	EventStarted     = "started"
	EventRunning     = "running"
	EventDone        = "done"
	EventRetired     = "retired"
	EventResumed     = "resumed"
	EventNeedsInput  = "needs_input"
	EventClosed      = "closed"
	EventMessage     = "message"
	EventExited      = "exited"
	EventHeartbeat   = "heartbeat"
)

// LifecycleStatus maps an event kind to the agent status it implies, or "".
func LifecycleStatus(kind string) string {
	switch kind {
	case EventStarted, EventRunning:
		return AgentRunning
	case EventDone, EventResumed:
		return AgentDone
	case EventRetired:
		return AgentRetired
	case EventNeedsInput:
		return AgentNeedsInput
	case EventExited:
		return AgentExited
	case EventClosed:
		return AgentClosed
	}
	return ""
}

var postableKinds = map[string]bool{
	EventStarted: true, EventRunning: true, EventDone: true,
	EventNeedsInput: true, EventClosed: true, EventExited: true, EventHeartbeat: true,
}

// PostableKind reports whether clients may post this event kind directly.
func PostableKind(kind string) bool { return postableKinds[kind] }

type Task struct {
	CleanupPending  int        `json:"cleanupPending"`
	Orchestrator    string     `json:"orchestrator"`
	Swarm           bool       `json:"swarm"`
	MaxNewAgents    int        `json:"maxNewAgents"`
	AllowAgentSpawn bool       `json:"allowAgentSpawn"`
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Goal            string     `json:"goal"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"createdAt"`
	CreatedBy       Caller     `json:"createdBy"`
	ClosedAt        *time.Time `json:"closedAt"`
}

type Agent struct {
	CleanupDone   bool      `json:"cleanupDone"`
	CleanupError  string    `json:"cleanupError,omitempty"`
	BlockedReason string    `json:"blockedReason,omitempty"`
	BlockedText   string    `json:"blockedText,omitempty"`
	RunID         string    `json:"runId"`
	LastSeenAt    time.Time `json:"lastSeenAt"`
	Online        bool      `json:"online"`
	ID            string    `json:"id"`
	TaskID        string    `json:"taskId"`
	Name          string    `json:"name"`
	Host          string    `json:"host"`
	Session       string    `json:"session"`
	Runtime       string    `json:"runtime"`
	Cwd           string    `json:"cwd"`
	ParentAgentID string    `json:"parentAgentId,omitempty"`
	Status        string    `json:"status"`
	Title         string    `json:"title"`
	CreatedAt     time.Time `json:"createdAt"`
	LastEventAt   time.Time `json:"lastEventAt"`
	Unread        int       `json:"unread"`
	ReadUpTo      int64     `json:"readUpTo"`
}

type Sender struct {
	AgentID string `json:"agentId,omitempty"`
	Node    string `json:"node"`
	User    string `json:"user"`
}

type Message struct {
	Broadcast bool      `json:"broadcast,omitempty"`
	ReplyTo   int64     `json:"replyTo,omitempty"`
	Seq       int64     `json:"seq"`
	TaskID    string    `json:"taskId"`
	From      Sender    `json:"from"`
	To        string    `json:"to,omitempty"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"createdAt"`
}

type Event struct {
	Seq       int64          `json:"seq"`
	TaskID    string         `json:"taskId"`
	Kind      string         `json:"kind"`
	AgentID   string         `json:"agentId,omitempty"`
	Text      string         `json:"text,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	By        Caller         `json:"by"`
	CreatedAt time.Time      `json:"createdAt"`
}

// Request and response bodies.

type CreateTaskRequest struct {
	Orchestrator    string `json:"orchestrator"`
	Swarm           bool   `json:"swarm"`
	MaxNewAgents    *int   `json:"maxNewAgents"`
	AllowAgentSpawn bool   `json:"allowAgentSpawn"`
	Name            string `json:"name"`
	Goal            string `json:"goal"`
}

type UpdateTaskRequest struct {
	Orchestrator    *string `json:"orchestrator"`
	Swarm           *bool   `json:"swarm"`
	MaxNewAgents    *int    `json:"maxNewAgents"`
	AllowAgentSpawn *bool   `json:"allowAgentSpawn"`
	Name            *string `json:"name"`
	Goal            *string `json:"goal"`
	Status          *string `json:"status"`
}

type AddAgentRequest struct {
	AgentID       string `json:"agentId"`
	Name          string `json:"name"`
	Host          string `json:"host"`
	Session       string `json:"session"`
	Runtime       string `json:"runtime"`
	Cwd           string `json:"cwd"`
	ParentAgentID string `json:"parentAgentId"`
}

type UpdateAgentRequest struct {
	Status *string `json:"status"`
	Title  *string `json:"title"`
}

type PostMessageRequest struct {
	ReplyTo int64  `json:"replyTo,omitempty"`
	Text    string `json:"text"`
	To      string `json:"to"`
	AgentID string `json:"agentId"`
}

type MarkReadRequest struct {
	AgentID string `json:"agentId"`
	UpTo    int64  `json:"upTo"`
}

type PostEventRequest struct {
	RunID   string         `json:"runId,omitempty"`
	Kind    string         `json:"kind"`
	AgentID string         `json:"agentId"`
	Text    string         `json:"text"`
	Data    map[string]any `json:"data"`
}

type TaskDetail struct {
	Task      Task    `json:"task"`
	Agents    []Agent `json:"agents"`
	LatestSeq int64   `json:"latestSeq"`
}

type TaskList struct {
	Tasks []Task `json:"tasks"`
}

type AgentList struct {
	Agents []Agent `json:"agents"`
}

type MessageList struct {
	Messages []Message `json:"messages"`
}

type EventList struct {
	Events []Event `json:"events"`
	Next   int64   `json:"next"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// Sentinel errors shared by store and server.
var (
	ErrNotFound           = errors.New("not found")
	ErrInvalid            = errors.New("invalid")
	ErrAgentSpawnLimit    = errors.New("maximum additional agents reached for this task")
	ErrAgentSpawnDisabled = errors.New("agents cannot add agents to this task")
	ErrLimit              = errors.New("limit reached")
	ErrClosed             = errors.New("closed")
)

type CleanupRequest struct {
	RunID string `json:"runId"`
	Error string `json:"error"`
}
