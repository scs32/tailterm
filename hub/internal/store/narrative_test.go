package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func narrativeFeature(t *testing.T, s *Store, ctx context.Context, by api.Caller, task api.Task, title, key string) api.WorkItem {
	t.Helper()
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: title, RequestID: key}, by)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func completeReportRequest(item api.WorkItem, key string, size int) api.PutNarrativeReportRequest {
	return api.PutNarrativeReportRequest{
		RequestID: key, ScopeRevision: item.ScopeRevision,
		Sections: api.NarrativeReportSections{
			RequestedOutcome: "Preserve the requested outcome.",
			DeliveredWork:    strings.Repeat("complete durable result ", size),
			Verification:     "Isolated store and API checks only; no deployment claim.",
			Limitations:      "Remote source completeness is not independently known.",
			RemainingWork:    "Release is separately sequenced.",
		},
		References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: item.TaskID, ItemID: item.ID, Revision: item.Revision, Label: "bounded feature scope"}},
	}
}

func TestNarrativeArtifactsLinksCoverageReportsRetriesAndCompletion(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "Narrative", "lead")
	item := narrativeFeature(t, s, ctx, by, project, "Queryable history", "narrative-item")
	message, err := s.PostMessage(ctx, project.ID, api.PostMessageRequest{Text: "Original retained request"}, by)
	if err != nil {
		t.Fatal(err)
	}
	artifactReq := api.PutNarrativeArtifactRequest{RequestID: "artifact-1", Namespace: "github-pr", SourceID: "repo#42", SourceVersion: "body@abc", Kind: "pr-body", Title: "Submitted PR body", Provenance: "manual-submission", CaptureState: "stored-content", Availability: "last-observed", Content: "original body", OriginalAuthor: api.Sender{Node: "github", User: "octocat"}}
	artifact, replay, err := s.PutNarrativeArtifact(ctx, project.ID, item.ID, artifactReq, by)
	if err != nil || replay || artifact.Version != 1 || artifact.ContentDigest == "" {
		t.Fatalf("artifact=%+v replay=%v err=%v", artifact, replay, err)
	}
	correct := artifactReq
	correct.RequestID = "artifact-correction"
	correct.ArtifactID = artifact.ArtifactID
	correct.ExpectedVersion = 1
	correct.SourceVersion = "body@def"
	correct.Content = "corrected body"
	corrected, _, err := s.PutNarrativeArtifact(ctx, project.ID, item.ID, correct, by)
	if err != nil || corrected.Version != 2 || corrected.SupersedesVersion != 1 {
		t.Fatalf("correction=%+v err=%v", corrected, err)
	}
	originalAgain, replay, err := s.PutNarrativeArtifact(ctx, project.ID, item.ID, artifactReq, by)
	if err != nil || !replay || originalAgain.Version != 1 || originalAgain.Content != "original body" {
		t.Fatalf("response-loss replay=%+v replay=%v err=%v", originalAgain, replay, err)
	}
	changed := artifactReq
	changed.Title = "different"
	if _, _, err = s.PutNarrativeArtifact(ctx, project.ID, item.ID, changed, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed retry=%v", err)
	}
	linkReq := api.PutNarrativeLinkRequest{RequestID: "link-1", Action: "link", Relationship: "requested-by", Target: api.NarrativeReference{Kind: "message", TaskID: project.ID, MessageSeq: message.Seq}, FeatureRevision: 1}
	link, _, err := s.PutNarrativeLink(ctx, project.ID, item.ID, linkReq, by)
	if err != nil || link.Revision != 1 {
		t.Fatalf("link=%+v err=%v", link, err)
	}
	retract := linkReq
	retract.RequestID = "link-retract"
	retract.LinkID = link.LinkID
	retract.ExpectedRevision = 1
	retract.Action = "retract"
	retract.Reason = "source relationship corrected"
	if got, _, err := s.PutNarrativeLink(ctx, project.ID, item.ID, retract, by); err != nil || got.Revision != 2 || got.Action != "retract" {
		t.Fatalf("retract=%+v err=%v", got, err)
	}
	coverageReq := api.PutNarrativeCoverageRequest{RequestID: "coverage-1", Source: "pr", Scope: "Selected body revisions only", CaptureState: "stored-content", CapturedIDs: []string{"body@abc", "body@def"}, KnownGaps: []string{"comments were not supplied"}, UnknownExtent: true, Assessment: "independently-verified", AssessmentText: "The submitted body versions were compared.", EvidenceReferences: []api.NarrativeReference{{Kind: "artifact-version", ArtifactID: artifact.ArtifactID, Version: 1, Label: "exact submitted body"}}, AssessedBy: api.Sender{Node: "independent-review", User: "verifier"}}
	coverage, _, err := s.PutNarrativeCoverage(ctx, project.ID, item.ID, coverageReq, by)
	if err != nil || coverage.Revision != 1 || !coverage.UnknownExtent || len(coverage.EvidenceReferences) != 1 || coverage.AssessmentBy.User != "verifier" {
		t.Fatalf("coverage=%+v err=%v", coverage, err)
	}
	unreferenced := coverageReq
	unreferenced.RequestID = "coverage-unreferenced"
	unreferenced.CoverageID = ""
	unreferenced.EvidenceReferences = nil
	if _, _, err = s.PutNarrativeCoverage(ctx, project.ID, item.ID, unreferenced, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("verified assessment without evidence=%v", err)
	}
	unattributed := coverageReq
	unattributed.RequestID = "coverage-unattributed"
	unattributed.AssessedBy = api.Sender{}
	if _, _, err = s.PutNarrativeCoverage(ctx, project.ID, item.ID, unattributed, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("verified assessment without verifier=%v", err)
	}
	reportReq := completeReportRequest(item, "report-1", 500)
	report, replay, err := s.PutNarrativeReport(ctx, project.ID, item.ID, reportReq, by)
	if err != nil || replay || len(report.Sections.DeliveredWork) <= 8192 || report.Digest == "" {
		t.Fatalf("report len=%d %+v replay=%v err=%v", len(report.Sections.DeliveredWork), report, replay, err)
	}
	exact, err := s.GetNarrativeReportVersion(ctx, project.ID, item.ID, report.ReportID, 1)
	if err != nil || exact.Sections.DeliveredWork != report.Sections.DeliveredWork || exact.Digest != report.Digest {
		t.Fatalf("exact report mismatch: %v", err)
	}
	done := "done"
	if _, err = s.UpdateWorkItem(ctx, project.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Status: &done}, by); !errors.Is(err, api.ErrNarrativeReportRequired) {
		t.Fatalf("legacy done without report=%v", err)
	}
	result, _, err := s.CreateWorkItemUpdate(ctx, project.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Status: &done, RequestID: "done-1", CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision}}, by)
	if err != nil || result.Revision.Status != "done" {
		t.Fatalf("done=%+v err=%v", result, err)
	}
	newReport := reportReq
	newReport.RequestID = "report-2"
	newReport.ReportID = report.ReportID
	newReport.ExpectedVersion = 1
	newReport.Sections.Limitations = "Corrected limitations after completion."
	correctedReport, _, err := s.PutNarrativeReport(ctx, project.ID, item.ID, newReport, by)
	if err != nil || correctedReport.Version != 2 {
		t.Fatalf("post-done correction=%+v err=%v", correctedReport, err)
	}
	overview, err := s.GetNarrativeOverview(ctx, project.ID, item.ID)
	if err != nil || overview.CompletionReport.Version != 1 || overview.LatestReport.Version != 2 || len(overview.Coverage) != 1 || len(overview.Coverage[0].EvidenceReferences) != 1 || overview.Coverage[0].AssessmentBy.User != "verifier" || len(overview.DefaultGaps) != 1 {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}
	if _, err = s.CloseTask(ctx, project.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetNarrativeReportVersion(ctx, project.ID, item.ID, report.ReportID, 1); err != nil {
		t.Fatalf("closed read=%v", err)
	}
}

func TestNarrativeFrozenPaginationConcurrencyAndStaleScope(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "Paging", "lead")
	item := narrativeFeature(t, s, ctx, by, project, "Frozen pages", "paging-item")
	for i := 0; i < 70; i++ {
		req := api.PutNarrativeArtifactRequest{RequestID: fmtKey("artifact", i), Namespace: "fixture", SourceID: fmtKey("source", i), SourceVersion: "1", Kind: "text", Title: fmtKey("Artifact", i), Provenance: "isolated-fixture", CaptureState: "stored-content", Availability: "available", Content: "fixture"}
		if _, _, err := s.PutNarrativeArtifact(ctx, project.ID, item.ID, req, by); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ListNarrativeArtifacts(ctx, project.ID, item.ID, "", 64)
	if err != nil || len(first.Artifacts) != 64 || first.NextCursor == "" {
		t.Fatalf("first page=%d cursor=%q err=%v", len(first.Artifacts), first.NextCursor, err)
	}
	timelineFirst, err := s.ListNarrativeTimeline(ctx, project.ID, item.ID, "", 64, "artifact", "fixture", "", "stored-content", "", "")
	if err != nil || len(timelineFirst.Entries) != 64 || timelineFirst.Cursor == "" {
		t.Fatalf("timeline first=%d cursor=%q err=%v", len(timelineFirst.Entries), timelineFirst.Cursor, err)
	}
	lateReq := api.PutNarrativeArtifactRequest{RequestID: "late", Namespace: "fixture", SourceID: "late", SourceVersion: "1", Kind: "text", Title: "late", Provenance: "isolated-fixture", CaptureState: "stored-content", Availability: "available", Content: "late"}
	late, _, err := s.PutNarrativeArtifact(ctx, project.ID, item.ID, lateReq, by)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ListNarrativeArtifacts(ctx, project.ID, item.ID, first.NextCursor, 64)
	if err != nil || len(second.Artifacts) != 6 {
		t.Fatalf("frozen second=%d err=%v", len(second.Artifacts), err)
	}
	for _, a := range second.Artifacts {
		if a.SourceID == "late" {
			t.Fatal("late append leaked into frozen traversal")
		}
	}
	timelineSecond, err := s.ListNarrativeTimeline(ctx, project.ID, item.ID, timelineFirst.Cursor, 64, "artifact", "fixture", "", "stored-content", "", "")
	if err != nil || len(timelineSecond.Entries) != 6 {
		t.Fatalf("timeline frozen second=%d err=%v", len(timelineSecond.Entries), err)
	}
	for _, entry := range timelineSecond.Entries {
		if entry.ObjectID == late.ArtifactID {
			t.Fatal("late append leaked into frozen timeline traversal")
		}
	}
	base := first.Artifacts[0].Latest
	original, err := s.GetNarrativeArtifactVersion(ctx, project.ID, item.ID, base.ArtifactID, base.Version)
	if err != nil {
		t.Fatal(err)
	}
	requests := []api.PutNarrativeArtifactRequest{}
	for i := 0; i < 2; i++ {
		requests = append(requests, api.PutNarrativeArtifactRequest{RequestID: fmtKey("race", i), ArtifactID: base.ArtifactID, ExpectedVersion: 1, Namespace: base.Namespace, SourceID: base.SourceID, SourceVersion: fmtKey("correction", i), Kind: base.Kind, Title: base.Title, Provenance: "isolated-fixture", CaptureState: "stored-content", Availability: "available", Content: original.Content + fmtKey("-", i)})
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, req := range requests {
		wg.Add(1)
		go func(req api.PutNarrativeArtifactRequest) {
			defer wg.Done()
			_, _, e := s.PutNarrativeArtifact(context.Background(), project.ID, item.ID, req, by)
			errs <- e
		}(req)
	}
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if errors.Is(e, api.ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("artifact CAS successes=%d conflicts=%d", success, conflict)
	}
	reportReq := completeReportRequest(item, "stale-report", 1)
	report, _, err := s.PutNarrativeReport(ctx, project.ID, item.ID, reportReq, by)
	if err != nil {
		t.Fatal(err)
	}
	title := "Changed scope"
	updated, err := s.UpdateWorkItem(ctx, project.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Title: &title}, by)
	if err != nil || updated.ScopeRevision != item.ScopeRevision+1 {
		t.Fatalf("scope update=%+v %v", updated, err)
	}
	done := "done"
	_, _, err = s.CreateWorkItemUpdate(ctx, project.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: updated.Revision, Status: &done, RequestID: "stale-done", CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: 1, Digest: report.Digest, ScopeRevision: report.ScopeRevision}}, by)
	if !errors.Is(err, api.ErrNarrativeReportStale) {
		t.Fatalf("stale completion=%v", err)
	}
}

func fmtKey(prefix string, n int) string { return prefix + "-" + strconv.Itoa(n) }

func TestNarrativeDecodedReportLimitAndBoundedOverviewCoverage(t *testing.T) {
	s, ctx, by := workItemStore(t)
	project, _ := workItemProject(t, s, ctx, by, "Bounds", "lead")
	item := narrativeFeature(t, s, ctx, by, project, "Decoded bounds", "bounds-item")
	req := completeReportRequest(item, "escaped-report", 1)
	fixed := len(req.Sections.RequestedOutcome) + len(req.Sections.Verification) + len(req.Sections.Limitations) + len(req.Sections.RemainingWork)
	req.Sections.DeliveredWork = strings.Repeat("<", api.MaxNarrativeContentBytes-fixed)
	if _, _, err := s.PutNarrativeReport(ctx, project.ID, item.ID, req, by); err != nil {
		t.Fatalf("decoded 1MiB report with JSON escaping rejected: %v", err)
	}
	req.RequestID = "escaped-report-too-large"
	req.Sections.DeliveredWork += "x"
	if _, _, err := s.PutNarrativeReport(ctx, project.ID, item.ID, req, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("decoded report above 1MiB=%v", err)
	}

	references := make([]api.NarrativeReference, 256)
	for i := range references {
		references[i] = api.NarrativeReference{Kind: "external", SourceID: strings.Repeat("s", 512), Locator: "https://example.invalid/" + strings.Repeat("p", 1700), Label: strings.Repeat("l", 512)}
	}
	for i := 0; i < 8; i++ {
		coverage := api.PutNarrativeCoverageRequest{RequestID: fmtKey("bounded-coverage", i), Source: fmtKey("source", i), Scope: strings.Repeat("q", 4096), CaptureState: "reference-only", CapturedIDs: []string{"submitted-enumeration"}, KnownGaps: []string{"unknown remote extent"}, UnknownExtent: true, Assessment: "unverified", AssessmentText: strings.Repeat("a", 8192), EvidenceReferences: references}
		if _, _, err := s.PutNarrativeCoverage(ctx, project.ID, item.ID, coverage, by); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.GetNarrativeOverviewPage(ctx, project.ID, item.ID, "", 32)
	if err != nil || first.CoverageNextCursor == "" || len(first.Coverage) >= 8 {
		t.Fatalf("bounded overview count=%d cursor=%q err=%v", len(first.Coverage), first.CoverageNextCursor, err)
	}
	raw, _ := json.Marshal(first)
	if len(raw) > api.MaxNarrativeResponseBytes {
		t.Fatalf("overview metadata=%d", len(raw))
	}
	coverageFirst, err := s.ListNarrativeCoverage(ctx, project.ID, item.ID, "", 32)
	if err != nil || coverageFirst.NextCursor == "" || len(coverageFirst.Coverage) >= 8 {
		t.Fatalf("bounded coverage count=%d cursor=%q err=%v", len(coverageFirst.Coverage), coverageFirst.NextCursor, err)
	}
	raw, _ = json.Marshal(coverageFirst)
	if len(raw) > api.MaxNarrativeResponseBytes {
		t.Fatalf("coverage metadata=%d", len(raw))
	}
	lateCoverage := api.PutNarrativeCoverageRequest{RequestID: "bounded-coverage-late", Source: "late-source", Scope: "appended after frozen overview page", CaptureState: "not-ingested", KnownGaps: []string{"late"}, UnknownExtent: true, Assessment: "unverified"}
	if _, _, err := s.PutNarrativeCoverage(ctx, project.ID, item.ID, lateCoverage, by); err != nil {
		t.Fatal(err)
	}
	second, err := s.GetNarrativeOverviewPage(ctx, project.ID, item.ID, first.CoverageNextCursor, 32)
	if err != nil || len(first.Coverage)+len(second.Coverage) != 8 {
		t.Fatalf("overview continuation total=%d err=%v", len(first.Coverage)+len(second.Coverage), err)
	}
	seenSources := map[string]bool{}
	for _, entry := range append(first.Coverage, second.Coverage...) {
		if entry.Source == "late-source" {
			t.Fatal("late coverage leaked into frozen overview traversal")
		}
		if seenSources[entry.Source] {
			t.Fatalf("overview cursor repeated source %q", entry.Source)
		}
		seenSources[entry.Source] = true
	}
	coverageSecond, err := s.ListNarrativeCoverage(ctx, project.ID, item.ID, coverageFirst.NextCursor, 32)
	if err != nil || len(coverageFirst.Coverage)+len(coverageSecond.Coverage) != 8 {
		t.Fatalf("coverage continuation total=%d err=%v", len(coverageFirst.Coverage)+len(coverageSecond.Coverage), err)
	}
	seenSources = map[string]bool{}
	for _, entry := range append(coverageFirst.Coverage, coverageSecond.Coverage...) {
		if entry.Source == "late-source" {
			t.Fatal("late coverage leaked into frozen metadata traversal")
		}
		if seenSources[entry.Source] {
			t.Fatalf("coverage cursor repeated source %q", entry.Source)
		}
		seenSources[entry.Source] = true
	}
}
