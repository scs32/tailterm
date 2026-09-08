package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInventoryReportsOnlyToolNamesAndStatus(t *testing.T) {
	script := `#!/usr/bin/env python3
import sys,json
for line in sys.stdin:
 r=json.loads(line)
 if r.get('method')=='initialize': out={}
 elif r.get('method')=='mcpServerStatus/list':
  assert r['params']['threadId']=='thread-test'
  out={'data':[{'name':'example','authStatus':'notLoggedIn','runtimeStatus':'authenticationRequired','tools':{'safe_tool':{'description':'PRIVATE DESCRIPTION','inputSchema':{'secret':'TOKEN'}}},'environment':{'TOKEN':'PRIVATE VALUE'}}],'nextCursor':None}
 else: continue
 print(json.dumps({'id':r['id'],'result':out}),flush=True)
`
	path := filepath.Join(t.TempDir(), "codex")
	if e := os.WriteFile(path, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	servers, e := readCodexInventory(ctx, path, "thread-test", true)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(servers)
	if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "TOKEN") || len(servers) != 1 || servers[0].Status != "authenticationRequired" || servers[0].Tools[0] != "safe_tool" {
		t.Fatal(string(raw))
	}
}
func TestInventoryTimeoutDoesNotWaitIndefinitely(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex")
	if e := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 10\n"), 0700); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, e := readCodexInventory(ctx, path, "", false); e == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("inspection did not stop")
	}
}
