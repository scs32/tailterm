package main

import (
	"flag"
	"os/exec"
	"strings"
	"testing"
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
