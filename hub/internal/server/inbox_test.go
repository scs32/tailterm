package server

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// wi_a31c079e98518efc (i2): the messages route pages both ways from any seq
// and can return only messages addressed to an agent, while latest=1 alone
// and after/limit/to keep their earlier meaning for existing clients.
func TestListMessagesPaging(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	task := c.task("paging")
	a, err := c.st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "alpha", Host: "h", Session: "alpha", Runtime: "codex"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "beta", Host: "h", Session: "beta", Runtime: "codex"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	// Ten messages: board-wide, to alpha, to beta, repeating.
	var all, forAlpha, toAlpha []int64
	for i := 0; i < 10; i++ {
		to := []string{"", a.ID, b.ID}[i%3]
		m, err := c.st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: fmt.Sprintf("message %d", i), To: to}, c.who)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, m.Seq)
		if to != b.ID {
			forAlpha = append(forAlpha, m.Seq)
		}
		if to == a.ID {
			toAlpha = append(toAlpha, m.Seq)
		}
	}
	base := "/v1/tasks/" + task.ID + "/messages?"
	seqs := func(query string) []int64 {
		t.Helper()
		var out api.MessageList
		if code := c.do("GET", base+query, nil, &out); code != 200 {
			t.Fatalf("GET %s: %d", query, code)
		}
		got := []int64{}
		for _, m := range out.Messages {
			got = append(got, m.Seq)
		}
		return got
	}
	for _, tc := range []struct {
		name, query string
		want        []int64
	}{
		{"latest alone (unchanged)", "latest=1&limit=3", all[7:]},
		{"after limit to (unchanged)", fmt.Sprintf("after=%d&limit=4&to=%s", all[0], a.ID), forAlpha[1:5]},
		{"after alone (unchanged)", fmt.Sprintf("after=%d", all[8]), all[9:]},
		{"before newest", fmt.Sprintf("before=%d&latest=1&limit=2", all[5]), all[3:5]},
		{"before oldest", fmt.Sprintf("before=%d&limit=2", all[5]), all[0:2]},
		{"after and before", fmt.Sprintf("after=%d&before=%d", all[2], all[6]), all[3:6]},
		{"latest keeps after", fmt.Sprintf("after=%d&latest=1&limit=100", all[6]), all[7:]},
		{"directed newest", fmt.Sprintf("to=%s&directed=1&latest=1&limit=2", a.ID), toAlpha[1:]},
		{"directed after", fmt.Sprintf("to=%s&directed=1&after=%d", a.ID, toAlpha[0]), toAlpha[1:]},
	} {
		if got := seqs(tc.query); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %s = %v, want %v", tc.name, tc.query, got, tc.want)
		}
	}
	for _, query := range []string{"directed=1", "directed=1&to=agt_missing", "before=-1", "before=abc"} {
		if code := c.do("GET", base+query, nil, nil); code != 400 {
			t.Errorf("GET %s: %d, want 400", query, code)
		}
	}
}
