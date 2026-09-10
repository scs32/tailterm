package server

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestNarrativeHTTPFullReportArtifactAndCompletionGate(t *testing.T) {
	c := newClient(t)
	task := c.task("narrative-http")
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "feature", Title: "Durable report", RequestID: "narrative-http-item"}, &item); code != 201 {
		t.Fatalf("create=%d", code)
	}
	path := "/v1/tasks/" + task.ID + "/work-items/" + item.ID + "/narrative"
	artifactReq := api.PutNarrativeArtifactRequest{RequestID: "http-artifact", Namespace: "ci", SourceID: "run-7/attempt-2/job-3", SourceVersion: "attempt-2", Kind: "ci-log-excerpt", Title: "Submitted CI excerpt", Provenance: "manual-submission", CaptureState: "stored-content", Availability: "last-observed", Content: strings.Repeat("safe <script> text\n", 5000)}
	var artifact api.NarrativeArtifactVersion
	if code := c.do("POST", path+"/artifacts", artifactReq, &artifact); code != 201 || artifact.ContentDigest == "" {
		t.Fatalf("artifact=%d %+v", code, artifact)
	}
	var artifacts api.NarrativeArtifactList
	if code := c.do("GET", path+"/artifacts?limit=32", nil, &artifacts); code != 200 || len(artifacts.Artifacts) != 1 || artifacts.Artifacts[0].Latest.Content != "" {
		t.Fatalf("artifact metadata=%d %+v", code, artifacts)
	}
	var exact api.NarrativeArtifactVersion
	if code := c.do("GET", path+"/artifacts/"+artifact.ArtifactID+"/versions/1", nil, &exact); code != 200 || exact.Content != artifactReq.Content {
		t.Fatalf("artifact exact=%d len=%d", code, len(exact.Content))
	}
	var timeline api.NarrativeTimelinePage
	if code := c.do("GET", path+"/timeline?kind=artifact&source=ci&captureState=stored-content", nil, &timeline); code != 200 || len(timeline.Entries) != 1 || timeline.Entries[0].ObjectID != artifact.ArtifactID {
		t.Fatalf("filtered timeline=%d %+v", code, timeline)
	}
	reportReq := api.PutNarrativeReportRequest{RequestID: "http-report", ScopeRevision: item.ScopeRevision, Sections: api.NarrativeReportSections{RequestedOutcome: "Store the complete result.", DeliveredWork: strings.Repeat("full report body ", 700), Verification: "HTTP acceptance in an isolated database.", Limitations: "No production deployment was asserted.", RemainingWork: "Release remains separately authorized."}, References: []api.NarrativeReference{{Kind: "artifact-version", ArtifactID: artifact.ArtifactID, Version: 1, Label: "submitted CI excerpt"}, {Kind: "work-item-revision", TaskID: task.ID, ItemID: item.ID, Revision: 1, Label: "feature scope"}}}
	var report api.NarrativeReportVersion
	if code := c.do("POST", path+"/reports", reportReq, &report); code != 201 || len(report.Sections.DeliveredWork) <= 8192 {
		t.Fatalf("report=%d len=%d %+v", code, len(report.Sections.DeliveredWork), report)
	}
	status := "done"
	var completionError api.ErrorResponse
	if code := c.do("PATCH", "/v1/tasks/"+task.ID+"/work-items/"+item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &status}, &completionError); code != 409 || completionError.Code != "report-required" {
		t.Fatalf("legacy gate=%d %+v", code, completionError)
	}
	update := api.CreateWorkItemUpdate{ExpectedRevision: 1, Status: &status, RequestID: "http-done", CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision}}
	var completed api.WorkItemUpdateResult
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items/"+item.ID+"/updates", update, &completed); code != 201 || completed.Revision.Status != "done" {
		t.Fatalf("complete=%d %+v", code, completed)
	}
	var overview api.NarrativeOverview
	if code := c.do("GET", path, nil, &overview); code != 200 || overview.CompletionReport == nil || overview.CompletionReport.Digest != report.Digest || len(overview.DefaultGaps) != 2 {
		t.Fatalf("overview=%d %+v", code, overview)
	}
	var replay api.NarrativeReportVersion
	if code := c.do("POST", path+"/reports", reportReq, &replay); code != 200 || replay.Digest != report.Digest || replay.ReportID != report.ReportID {
		t.Fatalf("report replay=%d %+v", code, replay)
	}
	escaped := reportReq
	escaped.RequestID = "http-report-escaped"
	escaped.ReportID = report.ReportID
	escaped.ExpectedVersion = 1
	fixed := len(escaped.Sections.RequestedOutcome) + len(escaped.Sections.Verification) + len(escaped.Sections.Limitations) + len(escaped.Sections.RemainingWork)
	escaped.Sections.DeliveredWork = strings.Repeat("<", api.MaxNarrativeContentBytes-fixed)
	var escapedReport api.NarrativeReportVersion
	if code := c.do("POST", path+"/reports", escaped, &escapedReport); code != 201 || len(escapedReport.Sections.DeliveredWork) != api.MaxNarrativeContentBytes-fixed {
		t.Fatalf("escaped report=%d len=%d", code, len(escapedReport.Sections.DeliveredWork))
	}
	reference := api.NarrativeReference{Kind: "external", SourceID: strings.Repeat("<", 512), Locator: "https://example.invalid/" + strings.Repeat("p", 1700), Label: strings.Repeat("<", 512)}
	refs := []api.NarrativeReference{}
	for len(refs) < 256 {
		candidate := append(append([]api.NarrativeReference{}, refs...), reference)
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(candidate); err != nil {
			t.Fatal(err)
		}
		if encoded.Len() > api.MaxNarrativeReferenceBytes {
			break
		}
		refs = candidate
	}
	if len(refs) == 0 || len(refs) == 256 {
		t.Fatalf("reference boundary was not exercised: %d", len(refs))
	}
	largest := escaped
	largest.RequestID = "http-report-largest-envelope"
	largest.ReportID = ""
	largest.ExpectedVersion = 0
	largest.References = refs
	goClient, err := api.NewClient(c.srv.URL, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := goClient.PutNarrativeReport(context.Background(), task.ID, item.ID, largest)
	if err != nil {
		t.Fatalf("Go client largest report write: %v", err)
	}
	readback, err := goClient.GetNarrativeReportVersion(context.Background(), task.ID, item.ID, stored.ReportID, stored.Version)
	if err != nil || readback.Sections.DeliveredWork != largest.Sections.DeliveredWork || len(readback.References) != len(refs) || readback.Digest != stored.Digest {
		t.Fatalf("Go client largest report readback refs=%d err=%v", len(readback.References), err)
	}
	coverageReq := api.PutNarrativeCoverageRequest{RequestID: "http-coverage-largest-envelope", Source: "pr", Scope: "deliberately submitted reference enumeration", CaptureState: "reference-only", UnknownExtent: true, Assessment: "unverified", EvidenceReferences: refs}
	coverage, err := goClient.PutNarrativeCoverage(context.Background(), task.ID, item.ID, coverageReq)
	if err != nil || len(coverage.EvidenceReferences) != len(refs) {
		t.Fatalf("Go client coverage envelope refs=%d err=%v", len(coverage.EvidenceReferences), err)
	}
	tooLarge := largest
	tooLarge.RequestID = "http-report-reference-metadata-too-large"
	tooLarge.References = append(append([]api.NarrativeReference{}, refs...), reference)
	if _, err = goClient.PutNarrativeReport(context.Background(), task.ID, item.ID, tooLarge); err == nil {
		t.Fatal("report reference metadata above the declared bound was accepted")
	}
	coverageReq.RequestID = "http-coverage-reference-metadata-too-large"
	coverageReq.EvidenceReferences = tooLarge.References
	if _, err = goClient.PutNarrativeCoverage(context.Background(), task.ID, item.ID, coverageReq); err == nil {
		t.Fatal("coverage reference metadata above the declared bound was accepted")
	}
}

func TestNarrativeHTTPRejectsUnsafeReferencesAndOversizeContent(t *testing.T) {
	c := newClient(t)
	task := c.task("narrative-invalid")
	var item api.WorkItem
	c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "feature", Title: "Boundaries", RequestID: "invalid-item"}, &item)
	path := "/v1/tasks/" + task.ID + "/work-items/" + item.ID + "/narrative"
	bad := api.PutNarrativeArtifactRequest{RequestID: "bad-url", Namespace: "external", SourceID: "x", SourceVersion: "1", Kind: "binary", Title: "unsafe", Provenance: "manual-submission", CaptureState: "reference-only", Availability: "unknown", Locator: "javascript:alert(1)"}
	if code := c.do("POST", path+"/artifacts", bad, nil); code != 400 {
		t.Fatalf("unsafe locator=%d", code)
	}
	oversize := bad
	oversize.RequestID = "too-large"
	oversize.CaptureState = "stored-content"
	oversize.Locator = ""
	oversize.Content = strings.Repeat("x", api.MaxNarrativeContentBytes+1)
	if code := c.do("POST", path+"/artifacts", oversize, nil); code != 400 {
		t.Fatalf("oversize content=%d", code)
	}
}
