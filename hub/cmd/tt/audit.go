package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type stringListFlag []string

func (s *stringListFlag) String() string         { return strings.Join(*s, ",") }
func (s *stringListFlag) Set(value string) error { *s = append(*s, value); return nil }

func parseRelatedItem(raw string) (api.MessageWorkItem, error) {
	at := strings.LastIndex(raw, "@")
	slash := strings.Index(raw, "/")
	if slash < 1 || at <= slash+1 || at == len(raw)-1 {
		return api.MessageWorkItem{}, fmt.Errorf("related item %q must be TASK/ITEM@REVISION", raw)
	}
	rev, err := strconv.ParseInt(raw[at+1:], 10, 64)
	task, item := raw[:slash], raw[slash+1:at]
	if err != nil || rev < 1 || !api.ValidID(task, "tsk") || !api.ValidID(item, "wi") {
		return api.MessageWorkItem{}, fmt.Errorf("invalid related item %q", raw)
	}
	return api.MessageWorkItem{ItemTaskID: task, ItemID: item, ItemRevision: rev, Relationship: "related"}, nil
}

func readAuditJSON(path string, out any) error {
	if path == "" {
		return errors.New("--file is required")
	}
	var data []byte
	var err error
	if path == "-" {
		data, err = os.ReadFile("/dev/stdin")
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return err
	}
	if len(data) > api.MaxBody {
		return errors.New("audit request file exceeds 64 KiB")
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid audit request JSON: %w", err)
	}
	return nil
}

func bindAuditActor(e env, agentID, runID *string) error {
	if e.agent == "" {
		return nil
	}
	if *agentID != "" && *agentID != e.agent {
		return errors.New("request agentId does not match this exact agent")
	}
	if *runID != "" && *runID != e.runID {
		return errors.New("request runId does not match this exact run")
	}
	*agentID, *runID = e.agent, e.runID
	return nil
}

func cmdCapabilities(e env, args []string) error {
	fs := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	out, err := c.Capabilities(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(out)
	} else {
		fmt.Printf("schema %d\nmessage-audit %v\naudit-export %v\nqueue %v\npolicy %s\n", out.SchemaVersion, out.MessageAudit.Versions, out.AuditExport.Versions, out.Queue.Versions, out.Policy.MessageAudit)
	}
	return nil
}

func cmdMessageAudit(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt message-audit get|history|changes|correct|resolve|associate|receipt")
	}
	sub := args[0]
	fs := flag.NewFlagSet("message-audit "+sub, flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	seq := fs.Int64("seq", 0, "message sequence")
	file := fs.String("file", "", "JSON request file, or - for stdin")
	cursor := fs.String("cursor", "", "opaque continuation cursor")
	checkpoint := fs.String("checkpoint", "", "completed feed checkpoint")
	after := fs.Int64("after-version", 0, "history version after which to read")
	limit := fs.Int("limit", api.DefaultMessageAuditPage, "page size")
	kind := fs.String("kind", "", "intake or work filter")
	requestID := fs.String("request-id", "", "mutation retry identity")
	operation := fs.String("operation", "", "correct or resolve")
	agentID := fs.String("agent", e.agent, "exact claimed agent id")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *task == "" {
		return errors.New("no task")
	}
	c, err := e.client(15 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(15 * time.Second)
	defer cancel()
	var out any
	switch sub {
	case "get":
		if *seq < 1 {
			return errors.New("--seq is required")
		}
		value, e2 := c.GetMessageAudit(ctx, *task, *seq)
		err = e2
		out = value
	case "history":
		if *seq < 1 {
			return errors.New("--seq is required")
		}
		value, e2 := c.ListMessageAuditHistory(ctx, *task, *seq, *after, *cursor, *limit)
		err = e2
		out = value
	case "changes":
		value, e2 := c.ListMessageAuditChanges(ctx, *task, api.MessageAuditChangeQuery{Cursor: *cursor, Checkpoint: *checkpoint, Limit: *limit, Kind: *kind})
		err = e2
		out = value
	case "correct":
		if *seq < 1 {
			return errors.New("--seq is required")
		}
		var req api.CorrectMessageAuditRequest
		if err = readAuditJSON(*file, &req); err == nil {
			err = bindAuditActor(e, &req.AgentID, &req.RunID)
		}
		if err == nil {
			var value api.MessageAuditMutationResult
			value, err = c.CorrectMessageAudit(ctx, *task, *seq, req)
			out = value
		}
	case "resolve":
		if *seq < 1 {
			return errors.New("--seq is required")
		}
		var req api.ResolveMessageAuditRequest
		if err = readAuditJSON(*file, &req); err == nil {
			err = bindAuditActor(e, &req.AgentID, &req.RunID)
		}
		if err == nil {
			var value api.MessageAuditMutationResult
			value, err = c.ResolveMessageAudit(ctx, *task, *seq, req)
			out = value
		}
	case "associate":
		var req api.CreateMessageAuditAssociationRequest
		if err = readAuditJSON(*file, &req); err == nil {
			err = bindAuditActor(e, &req.AgentID, &req.RunID)
		}
		if err == nil {
			var value api.MessageAuditAssociationResult
			value, err = c.CreateMessageAuditAssociation(ctx, *task, req)
			out = value
		}
	case "receipt":
		if *requestID == "" || (*operation != "correct" && *operation != "resolve" && *operation != "associate") {
			return errors.New("receipt requires --request-id and --operation correct|resolve|associate")
		}
		if *operation == "associate" {
			value, e2 := c.GetMessageAuditAssociationReceipt(ctx, *task, *requestID, *agentID)
			err = e2
			out = value
		} else {
			value, e2 := c.GetMessageAuditReceipt(ctx, *task, *requestID, *operation, *agentID)
			err = e2
			out = value
		}
	default:
		return fmt.Errorf("unknown message-audit command %q", sub)
	}
	if err != nil {
		return err
	}
	printJSON(out)
	return nil
}

func cmdAuditExport(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt audit-export create|download")
	}
	sub := args[0]
	fs := flag.NewFlagSet("audit-export "+sub, flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	requestID := fs.String("request-id", "", "stable creation identity")
	id := fs.String("id", "", "immutable export id")
	output := fs.String("output", "", "output JSON file")
	asJSON := fs.Bool("json", false, "JSON metadata output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *task == "" {
		return errors.New("no task")
	}
	c, err := e.client(60 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if sub == "create" {
		if *requestID == "" {
			return errors.New("--request-id is required")
		}
		out, e2 := c.CreateAuditExport(ctx, *task, api.CreateAuditExportRequest{RequestID: *requestID, FormatVersion: api.AuditExportFormatVersion})
		if e2 != nil {
			return e2
		}
		if *output != "" {
			if err = downloadAuditExport(ctx, c, *task, out.ID, *output); err != nil {
				return err
			}
		}
		if *asJSON || *output == "" {
			printJSON(out)
		} else {
			fmt.Printf("downloaded %s (%d bytes, sha256 %s)\n", out.ID, out.ByteCount, out.SHA256)
		}
		return nil
	}
	if sub == "download" {
		if *id == "" || *output == "" {
			return errors.New("download requires --id and --output")
		}
		if err = downloadAuditExport(ctx, c, *task, *id, *output); err != nil {
			return err
		}
		fmt.Printf("downloaded %s to %s\n", *id, *output)
		return nil
	}
	return fmt.Errorf("unknown audit-export command %q", sub)
}

func downloadAuditExport(ctx context.Context, c *api.Client, task, id, path string) error {
	if path == "-" {
		return errors.New("--output - is not supported; choose a file for verified atomic download")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("output file already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.partial")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	h := sha256.New()
	var offset int64
	expected := ""
	var expectedTotal int64 = -1
	for {
		chunk, e2 := c.GetAuditExportChunk(ctx, task, id, offset, api.MaxAuditExportChunkBytes)
		if e2 != nil {
			return e2
		}
		if chunk.ExportID != id || chunk.Offset != offset || len(chunk.Data) > api.MaxAuditExportChunkBytes || chunk.NextOffset != offset+int64(len(chunk.Data)) || chunk.Total < 0 || chunk.Total > api.MaxAuditExportBytes || chunk.Total < chunk.NextOffset {
			return errors.New("invalid audit export chunk boundary")
		}
		if expectedTotal < 0 {
			expectedTotal = chunk.Total
		} else if chunk.Total != expectedTotal {
			return errors.New("audit export total changed during download")
		}
		if expected == "" {
			expected = chunk.SHA256
		} else if expected != chunk.SHA256 {
			return errors.New("audit export digest changed during download")
		}
		if _, err = f.Write(chunk.Data); err != nil {
			return err
		}
		_, _ = h.Write(chunk.Data)
		offset = chunk.NextOffset
		if chunk.Complete {
			if offset != expectedTotal {
				return errors.New("incomplete audit export")
			}
			break
		}
		if len(chunk.Data) == 0 || offset == expectedTotal {
			return errors.New("audit export download did not advance")
		}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != expected {
		return fmt.Errorf("audit export digest mismatch: got %s want %s", got, expected)
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Link publishes atomically and fails if a destination appeared while the
	// download was running. Rename would silently replace it on Unix.
	if err = os.Link(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("output file already exists: %s", path)
		}
		return err
	}
	if err = os.Remove(tmp); err != nil {
		return err
	}
	ok = true
	return nil
}
