package main

import (
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestWrapPreservesArguments(t *testing.T) {
	text := "two words; $(printf injected) `printf injected` 'quoted'"
	command, err := wrapCommand([]string{"--", "printf", "%s", text})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", command).Output()
	if err != nil || string(out) != text {
		t.Fatalf("argument reparsed: %q %v", out, err)
	}
	command, err = wrapCommand([]string{"--shell", "printf explicit; printf shell"})
	out, runErr := exec.Command("/bin/sh", "-c", command).Output()
	if err != nil || runErr != nil || string(out) != "explicitshell" {
		t.Fatal("explicit shell broken")
	}
}

func TestPostHelpAndUnknownFlagsNeverContactHub(t *testing.T) {
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "unexpected request", 500)
	}))
	defer s.Close()
	e := env{hub: s.URL, task: "tsk_0000000000000001"}
	for _, args := range [][]string{{"--help"}, {"-h"}, {"hello", "--help"}} {
		if err := cmdPost(e, args); err != nil {
			t.Fatalf("help %v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"--recipient", "owner", "hello"}, {"hello", "--reply-to", "bad"}, {"hello", "--to"}} {
		if err := cmdPost(e, args); err == nil {
			t.Fatalf("invalid flags accepted: %v", args)
		}
	}
	if requests != 0 {
		t.Fatalf("help or invalid flags contacted hub %d times", requests)
	}
}

func TestPostHumanReplyAndLiteralHelp(t *testing.T) {
	var posted []api.PostMessageRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/messages") {
			http.Error(w, "unexpected route", 500)
			return
		}
		var body api.PostMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		posted = append(posted, body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Message{Seq: int64(len(posted))})
	}))
	defer s.Close()
	e := env{hub: s.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001"}
	for _, args := range [][]string{{"A poem\nfor you", "--reply-to", "5"}, {"--", "--help"}} {
		if err := cmdPost(e, args); err != nil {
			t.Fatal(err)
		}
	}
	if len(posted) != 2 || posted[0].To != "" || posted[0].ReplyTo != 5 || posted[0].Text != "A poem\nfor you" || posted[1].Text != "--help" {
		t.Fatalf("incorrect posts: %+v", posted)
	}
}

func TestInboxDistinguishesHumanFromAgentNamedOwner(t *testing.T) {
	m := api.Message{Seq: 5, From: api.Sender{User: "owner"}, Text: "A poem please"}
	human := formatMessage(m, nil)
	if !strings.Contains(human, "owner (human)") || !strings.Contains(human, "tt post --reply-to 5") {
		t.Fatal(human)
	}
	m.From.AgentID = "agt_0000000000000001"
	agent := formatMessage(m, map[string]string{m.From.AgentID: "owner"})
	if strings.Contains(agent, "human") || strings.Contains(agent, "omit --to") {
		t.Fatal(agent)
	}
}

func TestPostTrailingFlagsAndLiteralSeparator(t *testing.T) {
	for _, args := range [][]string{{"hello", "--to", "reviewer", "--reply-to", "17"}, {"--to=reviewer", "hello", "--reply-to=17"}} {
		fs := flag.NewFlagSet("post", flag.ContinueOnError)
		to := fs.String("to", "", "")
		reply := fs.Int64("reply-to", 0, "")
		if err := fs.Parse(postArgs(args)); err != nil {
			t.Fatal(err)
		}
		if *to != "reviewer" || *reply != 17 || strings.Join(fs.Args(), " ") != "hello" {
			t.Fatal("misrouted positional message")
		}
	}
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	to := fs.String("to", "", "")
	if err := fs.Parse(postArgs([]string{"--", "literal", "--to", "reviewer"})); err != nil {
		t.Fatal(err)
	}
	if *to != "" || strings.Join(fs.Args(), " ") != "literal --to reviewer" {
		t.Fatal("literal separator broken")
	}
}
