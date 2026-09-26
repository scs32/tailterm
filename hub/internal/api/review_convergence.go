package api

// ReviewMetadata is an explicit transition attached to a typed Board message.
// General reviews use REVIEW/RESULT; focused verification uses REQUEST/RESULT;
// the lead records a disposition on a NOTICE. Unstructured history is unknown.
type ReviewMetadata struct {
	Mode               string          `json:"mode"`
	Candidate          string          `json:"candidate,omitempty"`
	Blockers           []ReviewFinding `json:"blockers,omitempty"`
	Findings           []ReviewFinding `json:"findings,omitempty"`
	BlockerIDs         []string        `json:"blockerIds,omitempty"`
	Fix                string          `json:"fix,omitempty"`
	VerificationItemID string          `json:"verificationItemId,omitempty"`
	Disposition        string          `json:"disposition,omitempty"`
}

type ReviewFinding struct {
	ID          string `json:"id"`
	Criterion   string `json:"criterion,omitempty"`
	Regression  bool   `json:"regression,omitempty"`
	Baseline    string `json:"baseline,omitempty"`
	Candidate   string `json:"candidate,omitempty"`
	Command     string `json:"command,omitempty"`
	Output      string `json:"output,omitempty"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

type ReviewScope struct {
	ScopeRevision int64             `json:"scopeRevision"`
	ItemRevision  int64             `json:"itemRevision"`
	AssignmentSeq int64             `json:"assignmentSeq"`
	Criteria      map[string]string `json:"criteria"`
}

type ReviewRound struct {
	ActiveRequestSeq int64             `json:"activeRequestSeq,omitempty"`
	Number           int               `json:"number"`
	ScopeRevision    int64             `json:"scopeRevision"`
	RequestSeq       int64             `json:"requestSeq"`
	ResultSeq        int64             `json:"resultSeq,omitempty"`
	Candidate        string            `json:"candidate"`
	ReviewerID       string            `json:"reviewerId"`
	ReviewerRun      string            `json:"reviewerRun"`
	StartedAt        string            `json:"startedAt"`
	CompletedAt      string            `json:"completedAt,omitempty"`
	Verdicts         map[string]string `json:"verdicts,omitempty"`
	Blockers         []ReviewFinding   `json:"blockers,omitempty"`
}

type ReviewFollowUp struct {
	Finding    ReviewFinding `json:"finding"`
	ItemID     string        `json:"itemId"`
	MessageSeq int64         `json:"messageSeq"`
}

type FocusedReview struct {
	ActiveRequestSeq   int64    `json:"activeRequestSeq,omitempty"`
	RequestSeq         int64    `json:"requestSeq"`
	ResultSeq          int64    `json:"resultSeq,omitempty"`
	Candidate          string   `json:"candidate"`
	Fix                string   `json:"fix"`
	BlockerIDs         []string `json:"blockerIds"`
	ReviewerID         string   `json:"reviewerId"`
	ReviewerRun        string   `json:"reviewerRun"`
	VerificationItemID string   `json:"verificationItemId,omitempty"`
	Passed             bool     `json:"passed"`
}

type ReviewDisposition struct {
	Kind       string `json:"kind"`
	Candidate  string `json:"candidate"`
	MessageSeq int64  `json:"messageSeq"`
	AgentID    string `json:"agentId"`
	RunID      string `json:"runId"`
}

type ReviewConvergence struct {
	Dispositions []ReviewDisposition `json:"dispositions,omitempty"`
	ItemID       string              `json:"itemId"`
	History      string              `json:"history"` // recorded or unknown; never inferred zero
	Scopes       []ReviewScope       `json:"scopes"`
	Rounds       []ReviewRound       `json:"rounds"`
	FollowUps    []ReviewFollowUp    `json:"followUps"`
	Focused      []FocusedReview     `json:"focused"`
	Disposition  *ReviewDisposition  `json:"disposition,omitempty"`
}
