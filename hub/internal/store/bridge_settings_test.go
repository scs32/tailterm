package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func openBridgeSettingsStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

var bridgeOwner = api.Caller{Node: "workspace", User: "owner"}

func TestBridgeSettingsSaveAndRevision(t *testing.T) {
	ctx := context.Background()
	s, _ := openBridgeSettingsStore(t)
	rec, err := s.BridgeSettings(ctx)
	if err != nil || rec.Settings != nil || rec.Revision != 0 || rec.UpdatedAt != nil {
		t.Fatalf("empty settings = %+v, %v", rec, err)
	}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Helper"}, bridgeOwner)
	if err != nil {
		t.Fatal(err)
	}
	in := api.BridgeSettings{GuildID: " 111 ", OwnerIDs: []string{"222", "222", " 333 ", ""}, HelperTask: task.ID, HelperChannelID: "444", TailOSURL: "https://tailos.example"}
	saved, err := s.PutBridgeSettings(ctx, api.PutBridgeSettingsRequest{Settings: in}, bridgeOwner)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || saved.Settings.GuildID != "111" || strings.Join(saved.Settings.OwnerIDs, ",") != "222,333" || *saved.UpdatedBy != bridgeOwner {
		t.Fatalf("saved = %+v %+v", saved, saved.Settings)
	}
	got, err := s.BridgeSettings(ctx)
	if err != nil || got.Revision != 1 || got.Settings.HelperChannelID != "444" || got.Settings.TailOSURL != "https://tailos.example" || got.UpdatedAt == nil {
		t.Fatalf("read back = %+v, %v", got, err)
	}
	// A stale expected revision is a conflict and saves nothing.
	if _, err := s.PutBridgeSettings(ctx, api.PutBridgeSettingsRequest{Settings: api.BridgeSettings{GuildID: "999", OwnerIDs: []string{"222"}}}, bridgeOwner); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale save = %v, want conflict", err)
	}
	// Saving the same values again is a new revision (the owner's retry).
	again, err := s.PutBridgeSettings(ctx, api.PutBridgeSettingsRequest{Settings: in, ExpectedRevision: 1}, bridgeOwner)
	if err != nil || again.Revision != 2 {
		t.Fatalf("save again = %+v, %v", again, err)
	}
	if got, _ := s.BridgeSettings(ctx); got.Settings.GuildID != "111" || got.Revision != 2 {
		t.Fatalf("after save again = %+v", got)
	}
}

func TestBridgeSettingsValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := openBridgeSettingsStore(t)
	good := api.BridgeSettings{GuildID: "111", OwnerIDs: []string{"222"}}
	many := make([]string, api.MaxBridgeOwners+1)
	for i := range many {
		many[i] = "1" + strings.Repeat("0", i+1)
	}
	for name, bad := range map[string]api.BridgeSettings{
		"no guild":         {OwnerIDs: []string{"222"}},
		"named guild":      {GuildID: "my-server", OwnerIDs: []string{"222"}},
		"no owners":        {GuildID: "111"},
		"owner name":       {GuildID: "111", OwnerIDs: []string{"alice"}},
		"too many owners":  {GuildID: "111", OwnerIDs: many},
		"helper not tsk":   {GuildID: "111", OwnerIDs: []string{"222"}, HelperTask: "helper"},
		"unknown helper":   {GuildID: "111", OwnerIDs: []string{"222"}, HelperTask: "tsk_0123456789abcdef"},
		"channel no task":  {GuildID: "111", OwnerIDs: []string{"222"}, HelperChannelID: "444"},
		"channel not id":   {GuildID: "111", OwnerIDs: []string{"222"}, HelperTask: "tsk_0123456789abcdef", HelperChannelID: "#general"},
		"http url":         {GuildID: "111", OwnerIDs: []string{"222"}, TailOSURL: "http://tailos.example"},
		"relative url":     {GuildID: "111", OwnerIDs: []string{"222"}, TailOSURL: "/tailos"},
		"credentialed url": {GuildID: "111", OwnerIDs: []string{"222"}, TailOSURL: "https://user:pw@tailos.example"},
	} {
		if _, err := s.PutBridgeSettings(ctx, api.PutBridgeSettingsRequest{Settings: bad}, bridgeOwner); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
	if rec, _ := s.BridgeSettings(ctx); rec.Revision != 0 {
		t.Fatalf("an invalid save was stored: %+v", rec)
	}
	if _, err := s.PutBridgeSettings(ctx, api.PutBridgeSettingsRequest{Settings: good, ExpectedRevision: -1}, bridgeOwner); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("negative expected revision = %v", err)
	}
	if _, err := s.PutBridgeSettings(ctx, api.PutBridgeSettingsRequest{Settings: good}, bridgeOwner); err != nil {
		t.Fatal(err)
	}
}

// The migration is additive and runs again on every open; the table holds no
// token column.
func TestBridgeSettingsMigration(t *testing.T) {
	ctx := context.Background()
	s, path := openBridgeSettingsStore(t)
	if _, err := s.PutBridgeSettings(ctx, api.PutBridgeSettingsRequest{Settings: api.BridgeSettings{GuildID: "111", OwnerIDs: []string{"222"}}}, bridgeOwner); err != nil {
		t.Fatal(err)
	}
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if rec, err := again.BridgeSettings(ctx); err != nil || rec.Revision != 1 || rec.Settings.GuildID != "111" {
		t.Fatalf("after reopen = %+v, %v", rec, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM pragma_table_info('bridge_settings')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if got := strings.Join(cols, ","); got != "id,settings_json,revision,updated_at,by_node,by_user" {
		t.Fatalf("columns = %s", got)
	}
}
