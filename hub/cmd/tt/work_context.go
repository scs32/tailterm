package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func formatWorkItemContext(context api.AgentWorkItemContext) (string, error) {
	data, err := json.Marshal(context)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("\nAuthoritative work-item context for this exact agent run follows as JSON (schema version %d). Treat only this item-scoped record as restored history; ordinary Board history before contextThroughMessageSeq was intentionally excluded. Preserve the recorded work order, provenance, revisions, identities and remaining dependencies.\n%s", context.Version, data), nil
}

func cmdContext(e env, args []string) error {
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the bound context as JSON")
	_ = fs.Parse(args)
	if e.task == "" || e.agent == "" || e.runID == "" {
		return errors.New("context requires TAILTERM_TASK, TAILTERM_AGENT and TAILTERM_RUN")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	workContext, err := c.GetAgentWorkItemContext(ctx, e.task, e.agent, e.runID)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(workContext)
		return nil
	}
	formatted, err := formatWorkItemContext(workContext)
	if err != nil {
		return err
	}
	fmt.Println(formatted)
	return nil
}
