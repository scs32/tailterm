package api

import (
	"errors"
	"strings"
	"testing"
)

func TestInterventionKindsAreFixed(t *testing.T) {
	want := "release,nudge,decision-on-behalf,gate-fix,cleanup,diagnosis,status,other"
	if got := strings.Join(InterventionKinds, ","); got != want {
		t.Fatalf("kinds = %s", got)
	}
	for _, kind := range InterventionKinds {
		if !ValidInterventionKind(kind) {
			t.Fatalf("%s rejected", kind)
		}
	}
	for _, kind := range []string{"", "Nudge", "nudge ", "fix", "decision_on_behalf"} {
		if ValidInterventionKind(kind) {
			t.Fatalf("%q accepted", kind)
		}
	}
}

func TestValidateAndFormatIntervention(t *testing.T) {
	ok := CreateInterventionRequest{Kind: "gate-fix", ItemID: "wi_d55e7d8c840a3739", ProductItemID: "wi_1bf37d8870d5f955", Text: "Fixed the gate by hand", RequestID: "k"}
	if err := ValidateIntervention(ok); err != nil {
		t.Fatal(err)
	}
	if got := FormatIntervention(ok); got != "Owner intervention (gate-fix) on wi_d55e7d8c840a3739; product fix wi_1bf37d8870d5f955: Fixed the gate by hand" {
		t.Fatalf("format = %q", got)
	}
	unlinked := ok
	unlinked.ProductItemID = ""
	if err := ValidateIntervention(unlinked); err != nil {
		t.Fatal(err)
	}
	if got := FormatIntervention(unlinked); got != "Owner intervention (gate-fix) on wi_d55e7d8c840a3739: Fixed the gate by hand" {
		t.Fatalf("format = %q", got)
	}
	for name, mutate := range map[string]func(*CreateInterventionRequest){
		"kind":         func(r *CreateInterventionRequest) { r.Kind = "foo" },
		"item":         func(r *CreateInterventionRequest) { r.ItemID = "tsk_d55e7d8c840a3739" },
		"missing item": func(r *CreateInterventionRequest) { r.ItemID = "" },
		"product":      func(r *CreateInterventionRequest) { r.ProductItemID = "wi_short" },
		"blank":        func(r *CreateInterventionRequest) { r.Text = " \t\n" },
		"control":      func(r *CreateInterventionRequest) { r.Text = "a\x01b" },
		"invalid utf8": func(r *CreateInterventionRequest) { r.Text = "a\xffb" },
		"too long":     func(r *CreateInterventionRequest) { r.Text = strings.Repeat("x", MaxInterventionText+1) },
	} {
		bad := ok
		mutate(&bad)
		if err := ValidateIntervention(bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}
