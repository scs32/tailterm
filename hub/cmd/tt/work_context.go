package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
)

func formatWorkItemContext(context api.AgentWorkItemContext) (string, error) {
	if len(context.Bundle) > api.MaxAgentWorkItemContextBytes {
		return "", api.ErrContextLimit
	}
	digest := sha256.Sum256(context.Bundle)
	if !utf8.Valid(context.Bundle) || !json.Valid(context.Bundle) || context.Binding.ContextDigest != hex.EncodeToString(digest[:]) {
		return "", errors.New("restored work-item context does not match its immutable digest; upgrade the hub and host together")
	}
	data, err := api.MarshalAgentWorkItemContext(context)
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
		data, err := api.MarshalAgentWorkItemContext(workContext)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, string(data))
		return err
	}
	formatted, err := formatWorkItemContext(workContext)
	if err != nil {
		return err
	}
	fmt.Println(formatted)
	return nil
}

// Bound file and inline admission equally, before attempting registration.
func readPreparedWorkContext(inline, path string) ([]byte, error) {
	data := []byte(inline)
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("read prepared work-item context: %w", err)
		}
		defer file.Close()
		data, err = io.ReadAll(io.LimitReader(file, api.MaxAgentWorkItemContextBytes+1))
		if err != nil {
			return nil, err
		}
	}
	if len(data) > api.MaxAgentWorkItemContextBytes {
		return nil, api.ErrContextLimit
	}
	if !utf8.Valid(data) || !json.Valid(data) {
		return nil, errors.New("prepared work-item context is not valid UTF-8 JSON")
	}
	return data, nil
}
