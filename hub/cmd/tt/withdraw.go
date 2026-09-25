package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// cmdWithdraw selects an obligation without marking its recipient's inbox as
// delivered. The server remains authoritative for sender/run authorization.
func cmdWithdraw(e env, args []string) error {
	fs := flag.NewFlagSet("withdraw", flag.ContinueOnError)
	reason := fs.String("reason", "", "why the earlier request is superseded")
	obligation := fs.String("obligation", "", "exact obligation ID")
	// The ordinary form is `tt withdraw SEQ --reason T`.
	if len(args) > 1 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string{}, args[1:]...), args[0])
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &exitError{2, err}
	}
	if fs.NArg() > 1 || (fs.NArg() == 0 && *obligation == "") {
		return &exitError{2, errors.New("usage: tt withdraw SEQ --reason TEXT or tt withdraw --obligation ID --reason TEXT")}
	}
	why := strings.TrimSpace(*reason)
	if why == "" || !api.ValidText(why, 500) {
		return &exitError{2, errors.New("a nonblank reason of at most 500 characters is required")}
	}
	var seq int64
	if fs.NArg() == 1 {
		var err error
		seq, err = strconv.ParseInt(strings.TrimPrefix(fs.Arg(0), "#"), 10, 64)
		if err != nil || seq < 1 {
			return &exitError{2, errors.New("SEQ must be a message number")}
		}
	}
	if *obligation != "" && !api.ValidObligationID(*obligation) {
		return &exitError{2, errors.New("--obligation must be an obligation ID")}
	}
	if e.agent == "" || e.runID == "" {
		return errors.New("tt withdraw needs an agent session (TAILTERM_AGENT and TAILTERM_RUN)")
	}
	task, err := e.requireTask()
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	target := *obligation
	if seq > 0 {
		list, err := c.ListObligationsFrom(ctx, task, "", seq, seq)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return fmt.Errorf("no obligation for message #%d", seq)
		}
		if target == "" && len(list) != 1 {
			return fmt.Errorf("message #%d has %d obligations; use --obligation ID", seq, len(list))
		}
		found := false
		for _, o := range list {
			if o.ID == target {
				found = true
			}
		}
		if target != "" && !found {
			return fmt.Errorf("obligation %s does not belong to message #%d", target, seq)
		}
		if target == "" {
			target = list[0].ID
		}
	}
	sum := sha256.Sum256([]byte("withdraw\x00" + e.runID + "\x00" + target + "\x00" + why))
	req := api.ObligationWithdrawRequest{AgentID: e.agent, RunID: e.runID, Reason: why, RequestID: fmt.Sprintf("withdraw-%x", sum[:12])}
	o, err := c.WithdrawObligation(ctx, task, target, req)
	if err != nil {
		return err
	}
	fmt.Printf("#%d %s (%s)\n", o.MessageSeq, o.Outcome, o.ID)
	return nil
}
