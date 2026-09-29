package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// cmdPromptPolicy reads or sets a project's runtime prompt policy
// (docs/runtime-prompts.md). Setting it is the owner's.
func cmdPromptPolicy(e env, args []string) error {
	usage := errors.New("usage: tt prompt-policy get|set --task ID [--revision N --action KIND=ACTION ...] [--json]")
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		return usage
	}
	operation := args[0]
	fs := flag.NewFlagSet("prompt-policy "+operation, flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	revision := fs.Int64("revision", -1, "expected policy revision (required for set)")
	var actions stringListFlag
	fs.Var(&actions, "action", "KIND=ACTION (repeatable)")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || fs.NArg() != 0 {
		return errors.New("a valid --task project ID is required")
	}
	var req api.RuntimePromptPolicyRequest
	if operation == "set" {
		if err := requireOwnerSession(e, "tt prompt-policy set"); err != nil {
			return err
		}
		if *revision < 0 {
			return errors.New("tt prompt-policy set needs --revision, the revision shown by tt prompt-policy get")
		}
		if len(actions) == 0 {
			return errors.New("tt prompt-policy set needs at least one --action KIND=ACTION")
		}
		req = api.RuntimePromptPolicyRequest{ExpectedRevision: *revision, Actions: map[string]string{}, ActorAgentID: e.agent}
		for _, a := range actions {
			kind, action, ok := strings.Cut(a, "=")
			if !ok || !api.RuntimePromptActionAllowed(kind, action) {
				return fmt.Errorf("%s is not an allowed KIND=ACTION; see tt prompt-policy get", a)
			}
			req.Actions[kind] = action
		}
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	var out api.RuntimePromptPolicy
	if operation == "set" {
		out, err = c.SetRuntimePromptPolicy(ctx, *task, req)
	} else {
		out, err = c.RuntimePromptPolicy(ctx, *task)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(out)
		return nil
	}
	fmt.Printf("Runtime prompt policy for %s (revision %d):\n", out.TaskID, out.Revision)
	for _, k := range api.RuntimePromptKinds {
		fmt.Printf("  %-26s %-24s allowed: %s\n", k.Kind, out.Actions[k.Kind], strings.Join(k.Actions, ", "))
	}
	return nil
}
