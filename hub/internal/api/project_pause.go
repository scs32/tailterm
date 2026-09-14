package api

import "time"

const ProjectPauseCapabilityVersion = 1

const MaxProjectPauseTargets = 1024

const (
	PauseServiceNone        = "none"
	PauseServiceTransferred = "transferred"
	PauseServiceDetached    = "detached"
	PauseServiceUnresolved  = "unresolved"
)

type ProjectPauseTargetRequest struct {
	AgentID            string                `json:"agentId"`
	RunID              string                `json:"runId"`
	ServiceDisposition string                `json:"serviceDisposition"`
	HandoffNote        string                `json:"handoffNote,omitempty"`
	ServiceEvidence    *OperationalReference `json:"serviceEvidence,omitempty"`
}

type PauseProjectRequest struct {
	Version                     int                         `json:"version"`
	RequestID                   string                      `json:"requestId"`
	ExpectedLifecycleGeneration int64                       `json:"expectedLifecycleGeneration"`
	PreviousTeamID              string                      `json:"previousTeamId,omitempty"`
	Targets                     []ProjectPauseTargetRequest `json:"targets"`
}

type ProjectResumeOrchestrator struct {
	AgentID string `json:"agentId"`
	RunID   string `json:"runId"`
	Name    string `json:"name"`
}

type ResumeProjectRequest struct {
	Version                     int                       `json:"version"`
	RequestID                   string                    `json:"requestId"`
	ExpectedPauseGeneration     int64                     `json:"expectedPauseGeneration"`
	ExpectedLifecycleGeneration int64                     `json:"expectedLifecycleGeneration"`
	RetainedHandoffDigest       string                    `json:"retainedHandoffDigest"`
	SelectedTeamID              string                    `json:"selectedTeamId"`
	Orchestrator                ProjectResumeOrchestrator `json:"orchestrator"`
}

type ConfirmProjectResumeRequest struct {
	Version                     int    `json:"version"`
	RequestID                   string `json:"requestId"`
	ExpectedLifecycleGeneration int64  `json:"expectedLifecycleGeneration"`
	ResumeReceiptID             string `json:"resumeReceiptId"`
	AgentID                     string `json:"agentId"`
	RunID                       string `json:"runId"`
}

type ResolvePauseHandoffRequest struct {
	Version            int                   `json:"version"`
	RequestID          string                `json:"requestId"`
	PauseGeneration    int64                 `json:"pauseGeneration"`
	AgentID            string                `json:"agentId"`
	RunID              string                `json:"runId"`
	ServiceDisposition string                `json:"serviceDisposition"`
	HandoffNote        string                `json:"handoffNote"`
	ServiceEvidence    *OperationalReference `json:"serviceEvidence,omitempty"`
}

// ProjectPauseTarget is an immutable exact-run team snapshot plus the current
// service-handoff and cleanup state for that same target.
type ProjectPauseTarget struct {
	AgentID                     string                `json:"agentId"`
	RunID                       string                `json:"runId"`
	Name                        string                `json:"name"`
	Host                        string                `json:"host"`
	Session                     string                `json:"session"`
	Runtime                     string                `json:"runtime"`
	Cwd                         string                `json:"cwd"`
	ParentAgentID               string                `json:"parentAgentId,omitempty"`
	Role                        string                `json:"role,omitempty"`
	WorkItem                    *AgentWorkItemBinding `json:"workItem,omitempty"`
	OriginalStatus              string                `json:"originalStatus"`
	RequestedServiceDisposition string                `json:"requestedServiceDisposition"`
	ServiceDisposition          string                `json:"serviceDisposition"`
	HandoffNote                 string                `json:"handoffNote,omitempty"`
	ServiceVerified             bool                  `json:"serviceVerified"`
	ServiceEvidence             *OperationalReference `json:"serviceEvidence,omitempty"`
	CleanupDone                 bool                  `json:"cleanupDone"`
	CleanupError                string                `json:"cleanupError,omitempty"`
}

type ProjectPausePreviousTeam struct {
	TeamID           string               `json:"teamId,omitempty"`
	OrchestratorName string               `json:"orchestratorName,omitempty"`
	Members          []ProjectPauseTarget `json:"members"`
}

type ProjectResumeAdmission struct {
	ReceiptID      string                    `json:"receiptId"`
	SelectedTeamID string                    `json:"selectedTeamId"`
	Orchestrator   ProjectResumeOrchestrator `json:"orchestrator"`
	Pending        bool                      `json:"pending"`
}

type ProjectPauseReceipt struct {
	ID              string    `json:"id"`
	Operation       string    `json:"operation"`
	RequestID       string    `json:"requestId"`
	TaskID          string    `json:"taskId"`
	PauseGeneration int64     `json:"pauseGeneration"`
	CreatedAt       time.Time `json:"createdAt"`
}

type ProjectPauseStatus struct {
	Version               int                       `json:"version"`
	Replay                bool                      `json:"replay"`
	TaskID                string                    `json:"taskId"`
	State                 string                    `json:"state"`
	PauseGeneration       int64                     `json:"pauseGeneration"`
	LifecycleGeneration   int64                     `json:"lifecycleGeneration"`
	CleanupPending        int                       `json:"cleanupPending"`
	HandoffPending        int                       `json:"handoffPending"`
	RetainedHandoffDigest string                    `json:"retainedHandoffDigest,omitempty"`
	PausedAt              *time.Time                `json:"pausedAt,omitempty"`
	ResumedAt             *time.Time                `json:"resumedAt,omitempty"`
	Targets               []ProjectPauseTarget      `json:"targets"`
	PreviousTeam          *ProjectPausePreviousTeam `json:"previousTeam,omitempty"`
	ResumeAdmission       *ProjectResumeAdmission   `json:"resumeAdmission,omitempty"`
	Receipt               *ProjectPauseReceipt      `json:"receipt,omitempty"`
}
