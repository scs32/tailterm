package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// cmdSend posts a typed board message (docs/broker-phase-1.md). It validates
// the envelope locally and never contacts the hub when it is invalid.
func cmdSend(e env, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: tt send --kind KIND --subject \"plain English\" [--to AGENT] [fields...]")
		fmt.Fprintln(fs.Output(), "       tt send --file message.json   (a JSON envelope; - reads stdin)")
		fmt.Fprintln(fs.Output(), "Kinds: "+strings.Join(api.EnvelopeKinds, ", ")+". See docs/message-broker.md.")
		fmt.Fprintln(fs.Output(), "Example: tt send --kind result --to lead --subject \"Tests pass for empty recipient check\" \\")
		fmt.Fprintln(fs.Output(), "           --outcome done --status a1=pass --evidence \"e1: go test ./cmd/tt -> ok\" --ref commit=abc1234")
		fs.PrintDefaults()
	}
	file := fs.String("file", "", "JSON envelope file, or - for stdin")
	task := fs.String("task", e.task, "task id")
	to := fs.String("to", "", "recipient agent name or id")
	reply := fs.Int64("reply-to", 0, "message sequence being answered")
	links := registerLinkFlags(fs)
	requestID := fs.String("request-id", "", "stable retry identity; when omitted it is derived from the message, so an identical resend returns the original")
	var env api.Envelope
	fs.StringVar(&env.Kind, "kind", "", "message kind")
	fs.StringVar(&env.Subject, "subject", "", "plain-English subject, 10-120 characters, no IDs")
	fs.StringVar(&env.Due, "due", "", "duration such as 45m or 2h")
	b := &env.Body
	for name, target := range map[string]*string{
		"objective": &b.Objective, "ask": &b.Ask, "candidate": &b.Candidate, "scope": &b.Scope,
		"question": &b.Question, "outcome": &b.Outcome, "answer": &b.Answer, "reason": &b.Reason,
		"needs": &b.Needs, "resume-when": &b.ResumeWhen, "severity": &b.Severity, "summary": &b.Summary, "text": &b.Text,
	} {
		fs.StringVar(target, name, "", name+" field")
	}
	var owns, attachments, refs, acceptance, status, options, evidence stringListFlag
	fs.Var(&owns, "owns", "owned file or artifact (repeatable)")
	fs.Var(&attachments, "attachment", "file path or artifact id (repeatable)")
	fs.Var(&refs, "ref", "reference as key=value, such as commit=abc1234 (repeatable)")
	fs.Var(&acceptance, "acceptance", "criterion as a1=text (repeatable)")
	fs.Var(&status, "status", "criterion result as a1=pass|fail|partial (repeatable)")
	fs.Var(&options, "option", "question option as o1=text (repeatable)")
	fs.Var(&evidence, "evidence", `evidence as "e1: go test -> ok" or "e2 (commit): abc1234" (repeatable)`)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &exitError{2, err}
	}
	if fs.NArg() > 0 {
		return &exitError{2, fmt.Errorf("unexpected arguments %q; put text in a field such as --text", fs.Args())}
	}

	if *file != "" {
		var extra []string
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "file", "task", "to", "reply-to", "request-id",
				"work-item-task", "work-item", "work-item-revision", "work-order-task", "work-order-message", "related":
			default:
				extra = append(extra, "--"+f.Name)
			}
		})
		if len(extra) > 0 {
			return &exitError{2, fmt.Errorf("--file cannot be combined with %s; put those fields in the file", strings.Join(extra, ", "))}
		}
		var raw []byte
		var err error
		if *file == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(*file)
		}
		if err != nil {
			return &exitError{2, err}
		}
		fromFile := api.Envelope{}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&fromFile); err != nil {
			return &exitError{2, fmt.Errorf("envelope file: %w", err)}
		}
		env = fromFile
	} else {
		var err error
		b.Owns = owns
		env.Attachments = attachments
		if env.Refs, err = pairs("ref", refs, "="); err == nil {
			if b.Acceptance, err = pairs("acceptance", acceptance, "="); err == nil {
				if b.Status, err = pairs("status", status, "="); err == nil {
					b.Options, err = pairs("option", options, "=")
				}
			}
		}
		if err != nil {
			return &exitError{2, err}
		}
		for _, raw := range evidence {
			key, item, ok := api.ParseEvidenceEntry(raw)
			if !ok {
				return &exitError{2, fmt.Errorf(`--evidence %q must look like "e1: command -> outcome"`, raw)}
			}
			if env.Evidence == nil {
				env.Evidence = map[string]api.Evidence{}
			}
			env.Evidence[key] = item
		}
	}
	// One recipient: --to and the envelope's to must agree, and routing uses it.
	switch {
	case *to != "" && env.To != "" && *to != env.To:
		return &exitError{2, fmt.Errorf("--to %q conflicts with the message's to %q", *to, env.To)}
	case *to != "":
		env.To = *to
	}
	if strings.HasPrefix(env.To, "role:") {
		return &exitError{2, fmt.Errorf("role recipients such as %q are resolved in broker phase 2; name an agent", env.To)}
	}
	if problems := api.ValidateEnvelope(env); problems != nil {
		return &exitError{2, problemsError(problems)}
	}

	if *task == "" {
		return errors.New("no task")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	target := ""
	if env.To != "" {
		if target, err = resolveAgent(ctx, c, *task, env.To); err != nil {
			return err
		}
	}
	if *requestID == "" {
		raw, _ := json.Marshal(env)
		digest := sha256.Sum256([]byte(e.runID + "\x00" + target + "\x00" + fmt.Sprint(*reply) + "\x00" + string(raw)))
		*requestID = fmt.Sprintf("send-%x", digest[:12])
	}
	req := api.PostMessageRequest{Envelope: &env, To: target, AgentID: e.agent, ReplyTo: *reply}
	if err := links.apply(ctx, c, e, *task, target, *reply, api.RenderText(env), *requestID, false, &req); err != nil {
		return err
	}
	m, err := c.PostMessage(ctx, *task, req)
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && len(httpErr.Problems) > 0 {
		return &exitError{2, problemsError(httpErr.Problems)}
	}
	if err != nil {
		return err
	}
	fmt.Printf("posted #%d\n", m.Seq)
	return nil
}

func pairs(flagName string, values []string, sep string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, v := range values {
		k, val, ok := strings.Cut(v, sep)
		if !ok {
			return nil, fmt.Errorf("--%s %q must be key%svalue", flagName, v, sep)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return out, nil
}

func problemsError(problems []api.Problem) error {
	lines := make([]string, len(problems))
	for i, p := range problems {
		lines[i] = "  " + p.String()
	}
	return fmt.Errorf("invalid message:\n%s", strings.Join(lines, "\n"))
}
