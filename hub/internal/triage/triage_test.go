package triage

import (
	"reflect"
	"testing"
)

func TestTokensAndJaccard(t *testing.T) {
	if got := Tokens("The relay's status clears, the errors!"); !reflect.DeepEqual(got, []string{"clears", "errors", "relay", "status"}) {
		t.Fatalf("tokens %v", got)
	}
	if got := Similarity("Relay status clears errors", "relay status clears old errors"); got != 0.8 {
		t.Fatalf("similarity %v", got)
	}
	if Jaccard(nil, []string{"a"}) != 0 {
		t.Fatal("empty set scored")
	}
}

func TestCriteriaLines(t *testing.T) {
	description := "Intro text.\n\nAcceptance:\n- c1: queue ownership from intake\n- a2 (c1): unscoped add refused\n**b3.** handler briefing\nnot a criterion: prose"
	got := Criteria(description)
	want := []string{"queue ownership from intake", "unscoped add refused", "handler briefing"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("criteria %q", got)
	}
	if CriteriaSimilarity(description, "no criteria here") != 0 {
		t.Fatal("criteria scored against a description without criteria")
	}
}

func TestReleaseRows(t *testing.T) {
	record := "# Release\n\n| Tasks-hub | Item | What it does |\n|---|---|---|\n| `abc1234` | Relay status (`wi_3a2b456f2e997eb4`) | `tt relay --status` clears errors |\n| `def5678` | Docs | Task docs |\n\nText\n\n| A | B |\n|---|---|\n| x | y |\n"
	rows := ReleaseRows(record)
	if len(rows) != 3 || !reflect.DeepEqual(rows[0].ItemIDs, []string{"wi_3a2b456f2e997eb4"}) || rows[0].Cells[2] != "`tt relay --status` clears errors" || rows[2].Text != "x y" {
		t.Fatalf("rows %+v", rows)
	}
}
