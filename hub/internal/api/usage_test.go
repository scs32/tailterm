package api

import (
	"reflect"
	"testing"
)

func TestUsageNormalizedClassesPreserveRawAvailability(t *testing.T) {
	raw := map[string]int64{"input_tokens": 100, "cached_input_tokens": 80, "output_tokens": 10, "reasoning_output_tokens": 4}
	got, gap := NormalizeUsageTokens("codex", raw)
	want := map[string]int64{"input": 20, "cached": 80, "output": 6, "reasoning": 4}
	if gap != "" || !reflect.DeepEqual(got, want) || raw["input_tokens"] != 100 {
		t.Fatal(got, gap, raw)
	}
	if _, ok := got["cacheWrite"]; ok {
		t.Fatal("unavailable cache write inferred zero")
	}
	raw["cached_input_tokens"] = 120
	got, gap = NormalizeUsageTokens("codex", raw)
	if _, ok := got["input"]; ok || gap == "" {
		t.Fatal("invalid subset", got, gap)
	}
}
