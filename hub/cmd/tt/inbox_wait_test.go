package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestInboxWaitReturnsWhenDirectedMessageArrives(t *testing.T) {
	e, c, task, _ := cliWorkItemFixture(t)
	old := inboxPollInterval
	inboxPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { inboxPollInterval = old })

	out, err := captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--unread", "--wait", "150ms"}) })
	if err != nil || !strings.Contains(out, "(no messages)") {
		t.Fatalf("empty wait = %q, %v", out, err)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = c.PostMessage(context.Background(), task.ID, api.PostMessageRequest{Text: "review candidate abc123", To: e.agent})
	}()
	start := time.Now()
	out, err = captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--unread", "--wait", "5s"}) })
	if err != nil || !strings.Contains(out, "review candidate abc123") {
		t.Fatalf("wait output = %q, %v", out, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("wait did not return promptly: %v", time.Since(start))
	}
	if err := cmdInbox(env{hub: e.hub, task: e.task}, []string{"--unread", "--wait", "1s"}); err == nil {
		t.Fatal("wait without agent identity accepted")
	}
}
