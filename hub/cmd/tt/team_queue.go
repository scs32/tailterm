package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

func validTeamQueueEntryID(id string) bool {
	if !strings.HasPrefix(id, "tqe_") || len(id) != 20 {
		return false
	}
	_, err := hex.DecodeString(id[4:])
	return err == nil
}

func cmdTeamQueue(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt team queue add|list|remove|reorder")
	}
	sub := args[0]
	fs := flag.NewFlagSet("team queue "+sub, flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	hub := fs.String("hub", e.hub, "hub URL")
	item := fs.String("item", "", "work item ID")
	order := fs.Int64("order", 0, "recorded work-order message sequence")
	template := fs.String("template", "planned", "team template")
	entry := fs.String("entry", "", "queue entry ID")
	before := fs.String("before", "", "place before this queued entry; omit to move to end")
	cwd := fs.String("cwd", "", "absolute project folder for launch host")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || !api.ValidID(*task, "tsk") || *hub == "" {
		return errors.New("team queue requires a project and hub")
	}
	e.task, e.hub = *task, *hub
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if sub == "list" {
		list, err := c.ListTeamQueue(ctx, *task)
		if err != nil {
			return err
		}
		if *jsonOut {
			printJSON(list)
			return nil
		}
		if len(list.Entries) == 0 {
			fmt.Println("(empty team queue)")
			return nil
		}
		for _, q := range list.Entries {
			fmt.Printf("%d %s %s %s order=#%d revision=%d\n", q.Position, q.State, q.ID, q.ItemID, q.OrderMessageSeq, q.Revision)
		}
		return nil
	}
	if e.agent != "" {
		return errors.New("owner-side team queue changes require an unbound CLI session")
	}
	req := api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: sub}
	switch sub {
	case "add":
		if !api.ValidID(*item, "wi") || *order < 1 || *template != "planned" {
			return errors.New("usage: tt team queue add --item wi_ID --order SEQ [--template planned]")
		}
		if *cwd == "" {
			*cwd, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		*cwd, err = filepath.Abs(*cwd)
		if err != nil {
			return err
		}
		info, statErr := os.Stat(*cwd)
		if statErr != nil || !info.IsDir() {
			return fmt.Errorf("project cwd is not a directory: %s", *cwd)
		}
		req.ItemID, req.OrderMessageSeq, req.Template, req.Host, req.Cwd = *item, *order, *template, spawn.Host(), *cwd
	case "remove", "reorder":
		if !validTeamQueueEntryID(*entry) {
			return errors.New("remove/reorder requires --entry tqe_ID")
		}
		q, err := c.GetTeamQueueEntry(ctx, *task, *entry)
		if err != nil {
			return err
		}
		req.EntryID, req.ExpectedRevision, req.BeforeID = q.ID, q.Revision, *before
	default:
		return errors.New("usage: tt team queue add|list|remove|reorder")
	}
	result, err := c.TeamQueueAction(ctx, *task, req)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(result)
	} else {
		fmt.Printf("%s %s %s at %d\n", sub, result.ID, result.ItemID, result.Position)
	}
	return nil
}
