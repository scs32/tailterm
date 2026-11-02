package api

import (
	"context"
	"net/url"
	"time"
)

// The owner status call (bug wi_475a3df9b345e13e, owner #51832): the hub
// computes the owner report's figures (releases and today's count, the daily
// 21:00 baseline, opened and closed counts, host health and stalled teams)
// and serves them in one read, so no session keeps them in its notes.
// docs/owner-report.md, "Owner status call", defines every figure.

// OwnerStatusMaxSpan is the longest window one owner status read covers.
const OwnerStatusMaxSpan = 31 * 24 * time.Hour

// OwnerStatusBaselineHour is the local hour of the daily baseline.
const OwnerStatusBaselineHour = 21

// OwnerStatusQuery is the half-open window [From, To) and the IANA zone
// whose 21:00 is the daily baseline.
type OwnerStatusQuery struct {
	From, To time.Time
	Zone     string
}

// Query is the URL query of the read.
func (q OwnerStatusQuery) Query() url.Values {
	v := url.Values{}
	v.Set("from", q.From.UTC().Format(time.RFC3339Nano))
	v.Set("to", q.To.UTC().Format(time.RFC3339Nano))
	v.Set("tz", q.Zone)
	return v
}

type OwnerStatus struct {
	TaskID     string    `json:"taskId"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Zone       string    `json:"zone"`
	ComputedAt time.Time `json:"computedAt"`

	Releases OwnerStatusReleases `json:"releases"`
	Items    OwnerStatusItems    `json:"items"`
	// Baseline is the stored 21:00 baseline of the latest 21:00 in Zone
	// strictly before To; nil when that time is not yet past on the hub.
	Baseline *OwnerStatusBaseline `json:"baseline,omitempty"`
	// Host is the census of the queue head's host; nil when no host
	// reported one.
	Host  *OwnerStatusHost `json:"host,omitempty"`
	Teams OwnerStatusTeams `json:"teams"`
}

// OwnerStatusReleases counts release jobs by their settledAt in the window;
// InLine counts every job not yet settled. ReleasedSinceBaseline is nil
// without a baseline.
type OwnerStatusReleases struct {
	Released              int  `json:"released"`
	RolledBack            int  `json:"rolledBack"`
	Refused               int  `json:"refused"`
	Superseded            int  `json:"superseded"`
	InLine                int  `json:"inLine"`
	ReleasedSinceBaseline *int `json:"releasedSinceBaseline,omitempty"`
}

type OwnerStatusCounts struct {
	Bugs     int `json:"bugs"`
	Features int `json:"features"`
}

// OwnerStatusItems counts work items by kind. NetSinceBaseline is open now
// minus open at the baseline, nil without a baseline.
type OwnerStatusItems struct {
	Opened           OwnerStatusCounts  `json:"opened"`
	Closed           OwnerStatusCounts  `json:"closed"`
	OpenNow          OwnerStatusCounts  `json:"openNow"`
	NetSinceBaseline *OwnerStatusCounts `json:"netSinceBaseline,omitempty"`
}

// OwnerStatusBaseline is one stored daily baseline: written once, by the
// first read after its time, and never changed.
type OwnerStatusBaseline struct {
	Day           string            `json:"day"`
	At            time.Time         `json:"at"`
	Zone          string            `json:"zone"`
	ReleasedTotal int               `json:"releasedTotal"`
	Open          OwnerStatusCounts `json:"open"`
	RecordedAt    time.Time         `json:"recordedAt"`
}

// OwnerStatusHost is the host census as the queue stores it. A nil reading
// was not observed.
type OwnerStatusHost struct {
	Host           string `json:"host"`
	ObservedAt     string `json:"observedAt,omitempty"`
	FreeDiskMiB    *int64 `json:"freeDiskMiB,omitempty"`
	DiskReserveMiB int64  `json:"diskReserveMiB"`
	MemoryPressure *int   `json:"memoryPressure,omitempty"`
	StrainPressure int    `json:"strainPressure"`
	Strained       bool   `json:"strained,omitempty"`
	MatrixWaiters  *int   `json:"matrixWaiters,omitempty"`
}

// OwnerStatusTeams is the active team queue: launching and running entries,
// the concurrency limit, why the lanes admit nothing now (Hold, empty when
// they admit or the project has no lane policy) and every entry with a stall.
type OwnerStatusTeams struct {
	Running int                `json:"running"`
	Limit   int                `json:"limit"`
	Hold    string             `json:"hold,omitempty"`
	Stalled []OwnerStatusStall `json:"stalled"`
}

type OwnerStatusStall struct {
	EntryID string `json:"entryId"`
	ItemID  string `json:"itemId"`
	Cause   string `json:"cause"`
	Since   string `json:"since,omitempty"`
}

// OwnerStatus reads the project's owner status for one window.
func (c *Client) OwnerStatus(ctx context.Context, task string, q OwnerStatusQuery) (OwnerStatus, error) {
	var out OwnerStatus
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/owner-status?"+q.Query().Encode(), nil, &out)
}
