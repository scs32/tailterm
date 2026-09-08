package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"time"
)

// Retirement is task-wide hub state, so it also works for remote teammates.
// It deliberately does not terminate a process or inject terminal input.
func cmdRetirement(e env, operation string, args []string) error {
	fs := flag.NewFlagSet(operation, flag.ContinueOnError)
	task := fs.String("task", e.task, "task id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ref := e.agent
	if fs.NArg() > 1 {
		return errors.New("specify one agent name or ID")
	}
	if fs.NArg() == 1 {
		ref = fs.Arg(0)
	}
	if ref == "" || *task == "" {
		return fmt.Errorf("usage: tt %s [--task ID] [AGENT]", operation)
	}
	c, err := e.client(5 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(5 * time.Second)
	defer cancel()
	id, err := resolveAgent(ctx, c, *task, ref)
	if err != nil {
		return err
	}
	a, err := c.GetAgent(ctx, *task, id)
	if err != nil {
		return err
	}
	status := api.AgentRetired
	if operation == "resume" {
		if a.Status != api.AgentRetired {
			return errors.New("only a retired agent can be resumed; exited agents need a new launch")
		}
		status = api.AgentDone
	} else if a.Status == api.AgentRetired {
		fmt.Printf("%s is already retired.\n", a.Name)
		return nil
	}
	if _, err = c.UpdateAgent(ctx, *task, id, api.UpdateAgentRequest{Status: &status}); err != nil {
		return err
	}
	if operation == "resume" {
		fmt.Printf("Resumed %s. Automatic inbox wake-ups are enabled; unread messages remain available.\n", a.Name)
	} else {
		fmt.Printf("Retired %s. No further automatic inbox wake-ups; terminal and results retained. Any running or already queued turn may still finish.\n", a.Name)
	}
	return nil
}
