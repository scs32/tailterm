package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestCapabilitiesAndAuditExportHTTP(t *testing.T) {
	c := newClient(t)
	task := c.task("audit export HTTP")
	client, err := api.NewClient(c.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	caps, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if caps.SchemaVersion != 1 || caps.Policy.MessageAudit != "observe" || len(caps.AuditExport.Versions) != 2 || caps.AuditExport.Versions[0] != 2 || caps.AuditExport.Versions[1] != 3 || len(caps.Queue.Versions) != 1 || caps.Queue.Versions[0] != 1 {
		t.Fatalf("capabilities: %+v", caps)
	}
	created, err := client.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: "http-export", FormatVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	var bytes []byte
	for offset := int64(0); ; {
		chunk, e2 := client.GetAuditExportChunk(ctx, task.ID, created.ID, offset, 31)
		if e2 != nil {
			t.Fatal(e2)
		}
		bytes = append(bytes, chunk.Data...)
		offset = chunk.NextOffset
		if chunk.Complete {
			break
		}
	}
	h := sha256.Sum256(bytes)
	if hex.EncodeToString(h[:]) != created.SHA256 {
		t.Fatal("download digest mismatch")
	}
	replay, err := client.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: "http-export", FormatVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replay || replay.ID != created.ID {
		t.Fatalf("replay: %+v", replay)
	}
}
