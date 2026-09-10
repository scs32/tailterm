package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const narrativeUsage = "usage: tt work-items narrative <overview|timeline|artifacts|artifact-versions|artifact-get|artifact-put|links|link-put|coverage|coverage-put|reports|report-get|report-put|receipt> [flags] WI_ID"

func readNarrativeJSON(path string, out any) error {
	if path == "" {
		return errors.New("--file is required")
	}
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(api.MaxNarrativeRequestBody+1)))
	if err != nil {
		return err
	}
	if len(data) > api.MaxNarrativeRequestBody {
		return fmt.Errorf("request exceeds %d bytes", api.MaxNarrativeRequestBody)
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid narrative JSON: %w", err)
	}
	return nil
}

func narrativeItem(e env, fs *flag.FlagSet, projectFlag *string) (string, string, error) {
	if fs.NArg() != 1 || !api.ValidID(fs.Arg(0), "wi") {
		return "", "", errors.New(narrativeUsage)
	}
	project, err := workItemProject(e, *projectFlag)
	return project, fs.Arg(0), err
}

func narrativeClient(e env) (*api.Client, context.Context, context.CancelFunc, error) {
	c, err := e.client(30 * time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := ctxTimeout(30 * time.Second)
	return c, ctx, cancel, nil
}

func cmdWorkItemNarrative(e env, args []string) error {
	if len(args) == 0 {
		return errors.New(narrativeUsage)
	}
	name, rest := args[0], args[1:]
	if name == "artifact-put" || name == "link-put" || name == "coverage-put" || name == "report-put" {
		fs := flag.NewFlagSet("work-items narrative "+name, flag.ContinueOnError)
		projectFlag := fs.String("project", e.task, "owning project id")
		file := fs.String("file", "", "JSON request file, or - for stdin")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		project, item, err := narrativeItem(e, fs, projectFlag)
		if err != nil {
			return err
		}
		c, ctx, cancel, err := narrativeClient(e)
		if err != nil {
			return err
		}
		defer cancel()
		switch name {
		case "artifact-put":
			var req api.PutNarrativeArtifactRequest
			if err = readNarrativeJSON(*file, &req); err == nil {
				req.AgentID = e.agent
				req.RunID = e.runID
				var out api.NarrativeArtifactVersion
				out, err = c.PutNarrativeArtifact(ctx, project, item, req)
				if err == nil {
					printJSON(out)
				}
			}
		case "link-put":
			var req api.PutNarrativeLinkRequest
			if err = readNarrativeJSON(*file, &req); err == nil {
				req.AgentID = e.agent
				req.RunID = e.runID
				var out api.NarrativeLinkVersion
				out, err = c.PutNarrativeLink(ctx, project, item, req)
				if err == nil {
					printJSON(out)
				}
			}
		case "coverage-put":
			var req api.PutNarrativeCoverageRequest
			if err = readNarrativeJSON(*file, &req); err == nil {
				req.AgentID = e.agent
				req.RunID = e.runID
				var out api.NarrativeCoverageVersion
				out, err = c.PutNarrativeCoverage(ctx, project, item, req)
				if err == nil {
					printJSON(out)
				}
			}
		case "report-put":
			var req api.PutNarrativeReportRequest
			if err = readNarrativeJSON(*file, &req); err == nil {
				req.AgentID = e.agent
				req.RunID = e.runID
				var out api.NarrativeReportVersion
				out, err = c.PutNarrativeReport(ctx, project, item, req)
				if err == nil {
					printJSON(out)
				}
			}
		}
		return err
	}
	fs := flag.NewFlagSet("work-items narrative "+name, flag.ContinueOnError)
	projectFlag := fs.String("project", e.task, "owning project id")
	cursor := fs.String("cursor", "", "opaque continuation cursor")
	limit := fs.Int("limit", api.DefaultNarrativePage, "maximum entries (1-64)")
	kind := fs.String("kind", "", "timeline artifact kind")
	source := fs.String("source", "", "timeline source namespace")
	relationship := fs.String("relationship", "", "timeline relationship")
	captureState := fs.String("capture-state", "", "timeline capture state")
	sourceFrom := fs.String("source-from", "", "timeline source time at or after RFC3339")
	sourceTo := fs.String("source-to", "", "timeline source time at or before RFC3339")
	id := fs.String("id", "", "artifact or report id")
	version := fs.Int64("version", 0, "exact version")
	operation := fs.String("operation", "", "receipt operation")
	requestID := fs.String("request-id", "", "receipt request id")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	project, item, err := narrativeItem(e, fs, projectFlag)
	if err != nil {
		return err
	}
	c, ctx, cancel, err := narrativeClient(e)
	if err != nil {
		return err
	}
	defer cancel()
	var out any
	switch name {
	case "overview":
		out, err = c.GetNarrativeOverviewPage(ctx, project, item, *cursor, *limit)
	case "timeline":
		out, err = c.QueryNarrativeTimeline(ctx, project, item, api.NarrativeTimelineQuery{
			Cursor: *cursor, Limit: *limit, Kind: *kind, Source: *source,
			Relationship: *relationship, CaptureState: *captureState,
			SourceFrom: *sourceFrom, SourceTo: *sourceTo,
		})
	case "artifacts":
		out, err = c.ListNarrativeArtifacts(ctx, project, item, *cursor, *limit)
	case "artifact-versions":
		if *id == "" {
			return errors.New("--id is required")
		}
		out, err = c.ListNarrativeArtifactVersions(ctx, project, item, *id, *cursor, *limit)
	case "artifact-get":
		if *id == "" || *version < 1 {
			return errors.New("--id and --version are required")
		}
		out, err = c.GetNarrativeArtifactVersion(ctx, project, item, *id, *version)
	case "links":
		out, err = c.ListNarrativeLinks(ctx, project, item, *cursor, *limit)
	case "coverage":
		out, err = c.ListNarrativeCoverage(ctx, project, item, *cursor, *limit)
	case "reports":
		out, err = c.ListNarrativeReports(ctx, project, item, *cursor, *limit)
	case "report-get":
		if *id == "" || *version < 1 {
			return errors.New("--id and --version are required")
		}
		out, err = c.GetNarrativeReportVersion(ctx, project, item, *id, *version)
	case "receipt":
		if *requestID == "" || *operation == "" {
			return errors.New("--request-id and --operation are required")
		}
		out, err = c.GetNarrativeReceipt(ctx, project, item, *requestID, *operation, e.agent)
	default:
		return fmt.Errorf("unknown narrative command %q", name)
	}
	if err != nil {
		return err
	}
	printJSON(out)
	return nil
}
