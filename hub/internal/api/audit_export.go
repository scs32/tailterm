package api

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

const (
	AuditExportFormatVersion = 3
	AuditExportLegacyVersion = 2
	MaxAuditExportBytes      = 32 * 1024 * 1024
	MaxAuditExportChunkBytes = 256 * 1024
	MaxReadyAuditExports     = 8
	MaxReadyAuditExportBytes = 128 * 1024 * 1024
	AuditExportRetention     = 7 * 24 * time.Hour
)

type Capabilities struct {
	ProjectPause struct {
		Supported bool  `json:"supported"`
		Versions  []int `json:"versions"`
	} `json:"projectPause"`
	OperationalRecords struct {
		Supported bool  `json:"supported"`
		Versions  []int `json:"versions"`
	} `json:"operationalRecords"`
	SchemaVersion int `json:"schemaVersion"`
	MessageAudit  struct {
		Versions   []int `json:"versions"`
		MaxRelated int   `json:"maxRelated"`
	} `json:"messageAudit"`
	AuditExport struct {
		Versions         []int `json:"versions"`
		MaxBytes         int   `json:"maxBytes"`
		MaxChunkBytes    int   `json:"maxChunkBytes"`
		RetentionSeconds int64 `json:"retentionSeconds"`
	} `json:"auditExport"`
	Queue struct {
		Versions     []int `json:"versions"`
		DefaultPage  int   `json:"defaultPage"`
		MaxPage      int   `json:"maxPage"`
		MaxPageBytes int   `json:"maxPageBytes"`
	} `json:"queue"`
	Policy struct {
		MessageAudit string `json:"messageAudit"`
	} `json:"policy"`
	AllocationIntent struct {
		Supported bool  `json:"supported"`
		Versions  []int `json:"versions"`
	} `json:"allocationIntent"`
	ReliableDelivery struct {
		Supported bool  `json:"supported"`
		Versions  []int `json:"versions"`
	} `json:"reliableDelivery"`
}

// AllocationIntentCapabilityVersion is advertised here so a launch CLI can
// detect, before ever attempting a fresh parented item-bound tt spawn,
// whether the hub it is talking to supports and requires an allocation
// intent (independent review #2300/#2771/#2840/#2916 finding 3/6): a hub
// that predates this capability either omits the AllocationIntent section
// entirely or returns it with Supported=false, and a corrected CLI must
// fail closed with an explicit, actionable error rather than proceeding as
// if the old accounting model still applied.
const AllocationIntentCapabilityVersion = 1

func CurrentCapabilities() Capabilities {
	var out Capabilities
	out.SchemaVersion = 1
	out.ProjectPause.Supported = true
	out.ProjectPause.Versions = []int{ProjectPauseCapabilityVersion}
	out.MessageAudit.Versions = []int{1, 2}
	out.MessageAudit.MaxRelated = MaxMessageAuditRelated
	out.AuditExport.Versions = []int{AuditExportLegacyVersion, AuditExportFormatVersion}
	out.AuditExport.MaxBytes = MaxAuditExportBytes
	out.AuditExport.MaxChunkBytes = MaxAuditExportChunkBytes
	out.AuditExport.RetentionSeconds = int64(AuditExportRetention / time.Second)
	out.Policy.MessageAudit = "observe"
	out.Queue.Versions = []int{QueueVersion}
	out.Queue.DefaultPage = DefaultQueuePage
	out.Queue.MaxPage = MaxQueuePage
	out.Queue.MaxPageBytes = MaxQueuePageBytes
	out.AllocationIntent.Supported = true
	out.AllocationIntent.Versions = []int{AllocationIntentCapabilityVersion}
	// Broker phase 3 retired the legacy directive writers and operational
	// records; their history stays readable, but no client should use them.
	out.OperationalRecords.Supported = false
	out.ReliableDelivery.Supported = false
	return out
}

type CreateAuditExportRequest struct {
	RequestID     string `json:"requestId"`
	FormatVersion int    `json:"formatVersion"`
}

type AuditExportReceipt struct {
	ID        string    `json:"id"`
	RequestID string    `json:"requestId"`
	TaskID    string    `json:"taskId"`
	ExportID  string    `json:"exportId"`
	CreatedAt time.Time `json:"createdAt"`
}

type AuditExport struct {
	ID            string             `json:"id"`
	Schema        string             `json:"schema"`
	FormatVersion int                `json:"formatVersion"`
	SourceProject string             `json:"sourceProject"`
	RecordedBy    Caller             `json:"recordedBy"`
	CreatedAt     time.Time          `json:"createdAt"`
	ExpiresAt     time.Time          `json:"expiresAt"`
	ByteCount     int64              `json:"byteCount"`
	SHA256        string             `json:"sha256"`
	StreamCutoffs map[string]any     `json:"streamCutoffs"`
	Available     bool               `json:"available"`
	Receipt       AuditExportReceipt `json:"receipt"`
	Replay        bool               `json:"replay"`
}

type AuditExportChunk struct {
	ExportID   string `json:"exportId"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	Total      int64  `json:"total"`
	SHA256     string `json:"sha256"`
	Data       []byte `json:"data"`
	Complete   bool   `json:"complete"`
}

func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var out Capabilities
	err := c.do(ctx, "GET", "/v1/capabilities", nil, &out)
	return out, err
}

func (c *Client) CreateAuditExport(ctx context.Context, taskID string, req CreateAuditExportRequest) (AuditExport, error) {
	var out AuditExport
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(taskID)+"/audit-exports", req, &out)
	return out, err
}

func (c *Client) GetAuditExportChunk(ctx context.Context, taskID, exportID string, offset int64, limit int) (AuditExportChunk, error) {
	q := url.Values{}
	q.Set("offset", strconv.FormatInt(offset, 10))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out AuditExportChunk
	err := c.doLimited(ctx, "GET", "/v1/tasks/"+url.PathEscape(taskID)+"/audit-exports/"+url.PathEscape(exportID)+"?"+q.Encode(), nil, &out, 2*MaxAuditExportChunkBytes)
	return out, err
}
