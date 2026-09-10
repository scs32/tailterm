package api

import (
	"context"
	"net/url"
	"strconv"
)

func narrativePath(taskID, itemID, suffix string) string {
	return "/v1/tasks/" + taskID + "/work-items/" + itemID + "/narrative" + suffix
}

func narrativePageQuery(cursor string, limit int) string {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

func narrativeTimelineQuery(query NarrativeTimelineQuery) string {
	q := url.Values{}
	if query.Cursor != "" {
		q.Set("cursor", query.Cursor)
	}
	if query.Limit > 0 {
		q.Set("limit", strconv.Itoa(query.Limit))
	}
	for key, value := range map[string]string{
		"kind":         query.Kind,
		"source":       query.Source,
		"relationship": query.Relationship,
		"captureState": query.CaptureState,
		"sourceFrom":   query.SourceFrom,
		"sourceTo":     query.SourceTo,
	} {
		if value != "" {
			q.Set(key, value)
		}
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

func (c *Client) narrativeDo(ctx context.Context, method, path string, body, out any) error {
	return c.doLimited(ctx, method, path, body, out, MaxNarrativeEncodedBytes)
}

func (c *Client) GetNarrativeOverview(ctx context.Context, taskID, itemID string) (NarrativeOverview, error) {
	return c.GetNarrativeOverviewPage(ctx, taskID, itemID, "", DefaultNarrativePage)
}
func (c *Client) GetNarrativeOverviewPage(ctx context.Context, taskID, itemID, cursor string, limit int) (NarrativeOverview, error) {
	var out NarrativeOverview
	err := c.do(ctx, "GET", narrativePath(taskID, itemID, "")+narrativePageQuery(cursor, limit), nil, &out)
	return out, err
}
func (c *Client) ListNarrativeTimeline(ctx context.Context, taskID, itemID, cursor string, limit int) (NarrativeTimelinePage, error) {
	return c.QueryNarrativeTimeline(ctx, taskID, itemID, NarrativeTimelineQuery{Cursor: cursor, Limit: limit})
}
func (c *Client) QueryNarrativeTimeline(ctx context.Context, taskID, itemID string, query NarrativeTimelineQuery) (NarrativeTimelinePage, error) {
	var out NarrativeTimelinePage
	err := c.do(ctx, "GET", narrativePath(taskID, itemID, "/timeline")+narrativeTimelineQuery(query), nil, &out)
	return out, err
}
func (c *Client) ListNarrativeArtifacts(ctx context.Context, taskID, itemID, cursor string, limit int) (NarrativeArtifactList, error) {
	var out NarrativeArtifactList
	err := c.do(ctx, "GET", narrativePath(taskID, itemID, "/artifacts")+narrativePageQuery(cursor, limit), nil, &out)
	return out, err
}
func (c *Client) PutNarrativeArtifact(ctx context.Context, taskID, itemID string, req PutNarrativeArtifactRequest) (NarrativeArtifactVersion, error) {
	var out NarrativeArtifactVersion
	err := c.narrativeDo(ctx, "POST", narrativePath(taskID, itemID, "/artifacts"), req, &out)
	return out, err
}
func (c *Client) ListNarrativeArtifactVersions(ctx context.Context, taskID, itemID, artifactID, cursor string, limit int) (NarrativeArtifactVersionList, error) {
	var out NarrativeArtifactVersionList
	err := c.do(ctx, "GET", narrativePath(taskID, itemID, "/artifacts/"+url.PathEscape(artifactID)+"/versions")+narrativePageQuery(cursor, limit), nil, &out)
	return out, err
}
func (c *Client) GetNarrativeArtifactVersion(ctx context.Context, taskID, itemID, artifactID string, version int64) (NarrativeArtifactVersion, error) {
	var out NarrativeArtifactVersion
	err := c.narrativeDo(ctx, "GET", narrativePath(taskID, itemID, "/artifacts/"+url.PathEscape(artifactID)+"/versions/"+strconv.FormatInt(version, 10)), nil, &out)
	return out, err
}
func (c *Client) ListNarrativeLinks(ctx context.Context, taskID, itemID, cursor string, limit int) (NarrativeLinkList, error) {
	var out NarrativeLinkList
	err := c.do(ctx, "GET", narrativePath(taskID, itemID, "/links")+narrativePageQuery(cursor, limit), nil, &out)
	return out, err
}
func (c *Client) PutNarrativeLink(ctx context.Context, taskID, itemID string, req PutNarrativeLinkRequest) (NarrativeLinkVersion, error) {
	var out NarrativeLinkVersion
	err := c.do(ctx, "POST", narrativePath(taskID, itemID, "/links"), req, &out)
	return out, err
}
func (c *Client) ListNarrativeCoverage(ctx context.Context, taskID, itemID, cursor string, limit int) (NarrativeCoverageList, error) {
	var out NarrativeCoverageList
	err := c.do(ctx, "GET", narrativePath(taskID, itemID, "/coverage")+narrativePageQuery(cursor, limit), nil, &out)
	return out, err
}
func (c *Client) PutNarrativeCoverage(ctx context.Context, taskID, itemID string, req PutNarrativeCoverageRequest) (NarrativeCoverageVersion, error) {
	var out NarrativeCoverageVersion
	err := c.do(ctx, "POST", narrativePath(taskID, itemID, "/coverage"), req, &out)
	return out, err
}
func (c *Client) ListNarrativeReports(ctx context.Context, taskID, itemID, cursor string, limit int) (NarrativeReportList, error) {
	var out NarrativeReportList
	err := c.do(ctx, "GET", narrativePath(taskID, itemID, "/reports")+narrativePageQuery(cursor, limit), nil, &out)
	return out, err
}
func (c *Client) PutNarrativeReport(ctx context.Context, taskID, itemID string, req PutNarrativeReportRequest) (NarrativeReportVersion, error) {
	var out NarrativeReportVersion
	err := c.narrativeDo(ctx, "POST", narrativePath(taskID, itemID, "/reports"), req, &out)
	return out, err
}
func (c *Client) GetNarrativeReportVersion(ctx context.Context, taskID, itemID, reportID string, version int64) (NarrativeReportVersion, error) {
	var out NarrativeReportVersion
	err := c.narrativeDo(ctx, "GET", narrativePath(taskID, itemID, "/reports/"+url.PathEscape(reportID)+"/versions/"+strconv.FormatInt(version, 10)), nil, &out)
	return out, err
}
func (c *Client) GetNarrativeReceipt(ctx context.Context, taskID, itemID, requestID, operation, agentID string) (NarrativeReceipt, error) {
	q := url.Values{}
	q.Set("operation", operation)
	if agentID != "" {
		q.Set("agentId", agentID)
	}
	var out NarrativeReceipt
	err := c.narrativeDo(ctx, "GET", narrativePath(taskID, itemID, "/receipts/"+url.PathEscape(requestID))+"?"+q.Encode(), nil, &out)
	return out, err
}
