package bridge

import (
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
)

// Owner delegation windows from Discord (docs/owner-delegation-windows.md a3).
func TestDelegateCommands(t *testing.T) {
	h := newHarness(t)
	windows := func() []api.DelegationWindow {
		t.Helper()
		list, err := h.st.ListDelegationWindows(h.ctx, h.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return list.Windows
	}
	delegate := func(id, user, channel, scope string) string {
		in := discord.Interaction{ID: id, Type: discord.InteractionCommand, ChannelID: channel, Data: discord.InteractionData{Name: "delegate",
			Options: []discord.InteractionOption{stringOption("agent", "lead"), stringOption("until", "3h"), stringOption("scope", scope), stringOption("reason", "away")}}}
		if user != "" {
			in.Member = &discord.Member{User: &discord.User{ID: user}}
		}
		return editContent(h.interact(in))
	}
	// A stranger and an unlinked channel change nothing.
	h.interact(discord.Interaction{ID: "960000000000000001", Type: discord.InteractionCommand, Member: &discord.Member{User: &discord.User{ID: strangerID}},
		Data: discord.InteractionData{Name: "delegate", Options: []discord.InteractionOption{stringOption("agent", "lead"), stringOption("until", "3h"), stringOption("scope", "decisions")}}})
	if last := h.fake.callbacks[len(h.fake.callbacks)-1]; !strings.Contains(last.Response.Data.Content, "Only the project owner") {
		t.Fatalf("stranger /delegate answered %+v", last)
	}
	h.interact(discord.Interaction{ID: "960000000000000002", Type: discord.InteractionCommand, ChannelID: "999",
		Data: discord.InteractionData{Name: "delegate", Options: []discord.InteractionOption{stringOption("agent", "lead"), stringOption("until", "3h"), stringOption("scope", "decisions")}}})
	if last := h.fake.callbacks[len(h.fake.callbacks)-1]; !strings.Contains(last.Response.Data.Content, "isn't linked") {
		t.Fatalf("unlinked channel /delegate answered %+v", last)
	}
	if got := delegate("960000000000000003", "", "", "everything"); !strings.Contains(got, "Scope is decisions") {
		t.Fatalf("bad scope %q", got)
	}
	if len(windows()) != 0 {
		t.Fatal("rejected commands opened a window")
	}
	got := delegate("960000000000000004", "", "", "decisions-merges-deploys")
	if !strings.Contains(got, "lead answers your decisions, merges, deploys") {
		t.Fatalf("/delegate = %q", got)
	}
	ws := windows()
	if len(ws) != 1 || ws[0].State != api.DelegationOpen || ws[0].Scope != api.DelegationScopeDecisionsMergesDeploys || ws[0].OpenedSource == nil ||
		ws[0].OpenedSource.Kind != "discord" || ws[0].OpenedSource.ID != "960000000000000004" || ws[0].OpenedSource.UserID != ownerID || ws[0].OpenedBy.Node != api.BridgeNode {
		t.Fatalf("window %+v", ws)
	}
	// A redelivered interaction does nothing more.
	delegate("960000000000000004", "", "", "decisions-merges-deploys")
	if len(windows()) != 1 {
		t.Fatal("duplicate interaction opened another window")
	}
	if got := delegate("960000000000000005", "", "", "decisions"); !strings.Contains(got, "already open") {
		t.Fatalf("second /delegate = %q", got)
	}
	end := func(id string) string {
		return editContent(h.interact(discord.Interaction{ID: id, Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "delegate-end", Options: []discord.InteractionOption{stringOption("reason", "back")}}}))
	}
	if got := end("960000000000000006"); !strings.Contains(got, "Ended the delegation window to lead") {
		t.Fatalf("/delegate-end = %q", got)
	}
	ws = windows()
	if ws[0].State != api.DelegationClosed || ws[0].ClosedSource == nil || ws[0].ClosedSource.ID != "960000000000000006" || !strings.Contains(ws[0].EndReason, "back") {
		t.Fatalf("closed window %+v", ws[0])
	}
	if got := end("960000000000000007"); !strings.Contains(got, "No delegation window is open") {
		t.Fatalf("second /delegate-end = %q", got)
	}
}
