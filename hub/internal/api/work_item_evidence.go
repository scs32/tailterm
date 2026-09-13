package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// WorkItemEvidenceOperation selects one native work-item read contract. It is
// intentionally closed rather than accepting an arbitrary URL: callers cannot
// accidentally label a current-item response as history or substitute audit
// metadata for the requested evidence family.
type WorkItemEvidenceOperation string

const (
	WorkItemEvidenceCurrent   WorkItemEvidenceOperation = "current"
	WorkItemEvidenceRevisions WorkItemEvidenceOperation = "revisions"
	WorkItemEvidenceMessages  WorkItemEvidenceOperation = "messages"
	WorkItemEvidenceReceipt   WorkItemEvidenceOperation = "receipt"
)

type WorkItemEvidenceRead struct {
	Operation WorkItemEvidenceOperation
	TaskID    string
	ItemID    string
	After     int64
	Limit     int
	Revision  int64
	RequestID string
	AgentID   string
}

type RawWorkItemEvidence struct {
	StatusCode int
	Body       []byte
}

// ReadWorkItemEvidence returns the exact HTTP entity bytes for a supported
// native getter. Status codes are returned to the evidence reader so a 404 can
// be retained distinctly from transport and server errors.
func (c *Client) ReadWorkItemEvidence(ctx context.Context, read WorkItemEvidenceRead) (RawWorkItemEvidence, error) {
	if !ValidID(read.TaskID, "tsk") || !ValidID(read.ItemID, "wi") {
		return RawWorkItemEvidence{}, ErrInvalid
	}
	base := "/v1/tasks/" + read.TaskID + "/work-items/" + read.ItemID
	path := base
	switch read.Operation {
	case WorkItemEvidenceCurrent:
		if read.After != 0 || read.Limit != 0 || read.Revision != 0 || read.RequestID != "" || read.AgentID != "" {
			return RawWorkItemEvidence{}, ErrInvalid
		}
	case WorkItemEvidenceRevisions:
		if read.After < 0 || read.Limit < 1 || read.Limit > MaxWorkItemHistoryPage || read.Revision != 0 || read.RequestID != "" || read.AgentID != "" {
			return RawWorkItemEvidence{}, ErrInvalid
		}
		q := url.Values{}
		q.Set("after", strconv.FormatInt(read.After, 10))
		q.Set("limit", strconv.Itoa(read.Limit))
		path += "/revisions?" + q.Encode()
	case WorkItemEvidenceMessages:
		if read.After < 0 || read.Limit < 1 || read.Limit > MaxWorkItemHistoryPage || read.Revision < 0 || read.RequestID != "" || read.AgentID != "" {
			return RawWorkItemEvidence{}, ErrInvalid
		}
		q := url.Values{}
		q.Set("after", strconv.FormatInt(read.After, 10))
		q.Set("limit", strconv.Itoa(read.Limit))
		if read.Revision > 0 {
			q.Set("revision", strconv.FormatInt(read.Revision, 10))
		}
		path += "/messages?" + q.Encode()
	case WorkItemEvidenceReceipt:
		if read.After != 0 || read.Limit != 0 || read.Revision != 0 || read.RequestID == "" {
			return RawWorkItemEvidence{}, ErrInvalid
		}
		path += "/updates/receipts/" + url.PathEscape(read.RequestID)
		if read.AgentID != "" {
			q := url.Values{}
			q.Set("agentId", read.AgentID)
			path += "?" + q.Encode()
		}
	default:
		return RawWorkItemEvidence{}, fmt.Errorf("unsupported work-item evidence operation %q", read.Operation)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return RawWorkItemEvidence{}, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return RawWorkItemEvidence{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, MaxWorkItemHistoryBytes+1))
	if err != nil {
		return RawWorkItemEvidence{}, err
	}
	if len(body) > MaxWorkItemHistoryBytes {
		return RawWorkItemEvidence{}, fmt.Errorf("work-item evidence response exceeds %d bytes", MaxWorkItemHistoryBytes)
	}
	return RawWorkItemEvidence{StatusCode: res.StatusCode, Body: body}, nil
}
