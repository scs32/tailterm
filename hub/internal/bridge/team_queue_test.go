package bridge

import (
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestTeamQueueOnlyChangeRefreshesCardHash(t *testing.T) {
	task := api.Task{Name: "Project", PauseState: api.ProjectPauseActive}
	queue := api.TeamQueueList{Entries: []api.TeamQueueEntry{{ID: "tqe_1111111111111111", ItemID: "wi_2222222222222222", Position: 1, State: "queued"}}}
	_, before := renderCardWithQueue(task, nil, nil, queue)
	queue.Entries[0].State = "running"
	card, after := renderCardWithQueue(task, nil, nil, queue)
	if before == after {
		t.Fatal("queue-only state change did not change card hash")
	}
	if len(card.Embeds) != 1 || len(card.Embeds[0].Fields) != 1 || card.Embeds[0].Fields[0].Name != "Team queue" || !strings.Contains(card.Embeds[0].Fields[0].Value, "running") {
		t.Fatalf("queue missing from card: %+v", card)
	}
}
