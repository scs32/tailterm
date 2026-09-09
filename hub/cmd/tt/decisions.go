package main

import (
	"bytes"
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

// File contents describe a question, never the identity of its sender.
type askFileInput struct {
	api.DecisionRequest
	WorkItems        []api.MessageWorkItem `json:"workItems,omitempty"`
	WorkOrderMessage *api.MessageReference `json:"workOrderMessage,omitempty"`
}

var askRequestKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func readAskFile(path string) (askFileInput, error) {
	var input askFileInput
	var reader io.Reader = os.Stdin
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return input, err
		}
		defer file.Close()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, api.MaxBody+1))
	if err != nil {
		return input, err
	}
	if len(data) > api.MaxBody {
		return input, errors.New("decision JSON exceeds the request size limit")
	}
	return decodeAskFile(data)
}

func decodeAskFile(data []byte) (askFileInput, error) {
	var input askFileInput
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("invalid decision JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return input, errors.New("decision file must contain exactly one JSON object")
	}
	if err := api.ValidateDecisionRequest(input.DecisionRequest); err != nil {
		return input, err
	}
	return input, nil
}

func cmdAsk(e env, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: tt ask --request-id KEY --file PATH [--task ID] [--json]")
		fmt.Fprintln(fs.Output(), "Use --file - for stdin. JSON fields: question, options [{id,label,description}], recommendedOptionId, recommendationReason; optional workItems and workOrderMessage.")
		fmt.Fprintln(fs.Output(), "Reuse the same key and file after an uncertain response. Identity comes from this agent session. An owner answers on the Board; recommendations never submit consent.")
		fs.PrintDefaults()
	}
	key := fs.String("request-id", "", "stable retry key (required)")
	file := fs.String("file", "", "decision JSON file, or - for stdin (required)")
	task := fs.String("task", e.task, "current project id")
	asJSON := fs.Bool("json", false, "print the stored message as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || *file == "" || !askRequestKey.MatchString(*key) {
		return errors.New("usage: tt ask --request-id KEY --file PATH; key must be 1–128 ASCII letters/digits or -_.:")
	}
	if !api.ValidID(e.agent, "agt") || !api.ValidID(*task, "tsk") || *task != e.task {
		return errors.New("ask requires this session's current project and agent identity")
	}
	input, err := readAskFile(*file)
	if err != nil {
		return err
	}
	if len(input.WorkItems) > 1 || (input.WorkOrderMessage != nil && len(input.WorkItems) == 0) {
		return errors.New("decision context supports one primary item; an order reference requires that item")
	}
	if len(input.WorkItems) == 1 {
		item := input.WorkItems[0]
		if item.ItemTaskID != *task || !api.ValidID(item.ItemID, "wi") || item.ItemRevision < 1 || item.Relationship != "primary" {
			return errors.New("decision context requires a valid same-project primary item and revision")
		}
	}
	if order := input.WorkOrderMessage; order != nil && (order.TaskID != *task || order.Seq < 1) {
		return errors.New("decision order reference must identify a message in this project")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	message, err := c.CreateDecision(ctx, *task, api.CreateDecisionRequest{
		DecisionRequest: input.DecisionRequest, AgentID: e.agent, RequestID: *key,
		WorkItems: input.WorkItems, WorkOrderMessage: input.WorkOrderMessage,
	})
	if err != nil {
		var response *api.HTTPError
		if errors.As(err, &response) && response.Status < 500 {
			return fmt.Errorf("decision request rejected: %w", err)
		}
		return fmt.Errorf("%w; storage is uncertain. Retry tt ask with the same --request-id %s and unchanged file to recover the original request", err, *key)
	}
	if *asJSON {
		printJSON(message)
	} else {
		fmt.Printf("Decision requested #%d. The owner can answer on the Board.\n", message.Seq)
	}
	return nil
}
