package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"io"
	"os"
	"time"
)

func requireOperationalRecords(ctx context.Context, c *api.Client) error {
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("operational records unavailable: %w", err)
	}
	if caps.OperationalRecords.Supported {
		for _, v := range caps.OperationalRecords.Versions {
			if v == api.OperationalRecordsCapabilityVersion {
				return nil
			}
		}
	}
	return errors.New("hub lacks operationalRecords v1; upgrade before retrying; prose fallback is not supported")
}
func cmdOperationalRecord(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt operational-record propose|commit|get")
	}
	op := args[0]
	fs := flag.NewFlagSet("operational-record "+op, flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	file := fs.String("file", "", "typed JSON request file (- for stdin)")
	version := fs.Int64("version", 0, "immutable version for get; latest when omitted")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *task == "" {
		return errors.New("project id required")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if err = requireOperationalRecords(ctx, c); err != nil {
		return err
	}
	checkActor := func(agent, run string) error {
		if agent != e.agent || run != e.runID {
			return errors.New("request actor must match exact CLI agent/run")
		}
		return nil
	}
	switch op {
	case "propose":
		if *file == "" || fs.NArg() != 0 {
			return errors.New("propose requires --file and no positional arguments")
		}
		var req api.ProposeOperationalRecordRequest
		if err = readOperationalJSON(*file, &req); err != nil {
			return err
		}
		if req.AgentID == "" && req.RunID == "" {
			req.AgentID, req.RunID = e.agent, e.runID
		}
		if err = checkActor(req.AgentID, req.RunID); err != nil {
			return err
		}
		out, err := c.ProposeOperationalRecord(ctx, *task, req)
		if err != nil {
			return err
		}
		printJSON(out)
	case "commit":
		if *file == "" || fs.NArg() != 1 {
			return errors.New("commit requires RECORD_ID and --file")
		}
		var req api.CommitOperationalRecordRequest
		if err = readOperationalJSON(*file, &req); err != nil {
			return err
		}
		if req.AgentID == "" && req.RunID == "" {
			req.AgentID, req.RunID = e.agent, e.runID
		}
		if err = checkActor(req.AgentID, req.RunID); err != nil {
			return err
		}
		out, err := c.CommitOperationalRecord(ctx, *task, fs.Arg(0), req)
		if err != nil {
			return err
		}
		printJSON(out)
	case "get":
		if fs.NArg() != 1 || *version < 0 {
			return errors.New("get requires RECORD_ID and optional positive --version")
		}
		out, err := c.GetOperationalRecord(ctx, *task, fs.Arg(0), *version)
		if err != nil {
			return err
		}
		printJSON(out)
	default:
		return errors.New("usage: tt operational-record propose|commit|get")
	}
	return nil
}

func readOperationalJSON(path string, v any) error {
	var r io.Reader
	var f *os.File
	var err error
	if path == "-" {
		r = os.Stdin
	} else {
		f, err = os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	dec := json.NewDecoder(io.LimitReader(r, 128<<10))
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err != nil {
		return fmt.Errorf("invalid operational record schema: %w", err)
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return errors.New("expected exactly one operational record")
	}
	return nil
}
