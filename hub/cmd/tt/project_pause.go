package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

var projectPauseReceiptIDPattern = regexp.MustCompile(`^ppr_[0-9a-f]{16}$`)

func readProjectPauseRequest(path string, value any) error {
	if path == "" {
		return errors.New("--file is required")
	}
	var reader io.Reader = os.Stdin
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, api.MaxBody+1))
	if err != nil {
		return err
	}
	if len(data) > api.MaxBody {
		return errors.New("project pause request is too large")
	}
	if err = json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("invalid project pause request: %w", err)
	}
	return nil
}

func cmdProjectPause(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt project-pause get|pause|handoff|resume [--task ID] [--file PATH] [--json]")
	}
	operation := args[0]
	fs := flag.NewFlagSet("project-pause "+operation, flag.ContinueOnError)
	taskID := fs.String("task", e.task, "project ID")
	file := fs.String("file", "", "JSON request path; use - for stdin")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || !api.ValidID(*taskID, "tsk") {
		return errors.New("a valid --task project ID and no positional arguments are required")
	}
	if operation == "get" && *file != "" {
		return errors.New("project-pause get does not accept --file")
	}
	if operation != "get" && *file == "" {
		return errors.New("project-pause mutation requires --file")
	}
	c, err := e.client(15 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out api.ProjectPauseStatus
	switch operation {
	case "get":
		out, err = c.GetProjectPause(ctx, *taskID)
	case "pause":
		var req api.PauseProjectRequest
		if err = readProjectPauseRequest(*file, &req); err == nil {
			out, err = c.PauseProject(ctx, *taskID, req)
		}
	case "handoff":
		var req api.ResolvePauseHandoffRequest
		if err = readProjectPauseRequest(*file, &req); err == nil {
			out, err = c.ResolvePauseHandoff(ctx, *taskID, req)
		}
	case "resume":
		var req api.ResumeProjectRequest
		if err = readProjectPauseRequest(*file, &req); err == nil {
			out, err = c.ResumeProject(ctx, *taskID, req)
		}
	default:
		return errors.New("usage: tt project-pause get|pause|handoff|resume [--task ID] [--file PATH] [--json]")
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(out)
		return nil
	}
	fmt.Printf("Project %s: %s, pause generation %d, lifecycle generation %d, cleanup pending %d, handoff pending %d\n", out.TaskID, out.State, out.PauseGeneration, out.LifecycleGeneration, out.CleanupPending, out.HandoffPending)
	if out.Receipt != nil {
		fmt.Printf("Receipt %s (%s request %s)\n", out.Receipt.ID, out.Receipt.Operation, out.Receipt.RequestID)
	}
	return nil
}
