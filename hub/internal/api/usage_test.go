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

func TestUsageRealShapedClassesAndUncertainCodexWriteOverlap(t *testing.T) {
	codex := map[string]int64{"input_tokens": 100, "cached_input_tokens": 80, "cache_write_input_tokens": 0, "output_tokens": 10, "reasoning_output_tokens": 4, "total_tokens": 110}
	got, gap := NormalizeUsageTokens("codex", codex)
	if gap != "" || len(got) != 5 || got["input"] != 20 || got["cacheWrite"] != 0 || got["output"] != 6 {
		t.Fatal(got, gap)
	}
	codex["cache_write_input_tokens"] = 3
	got, gap = NormalizeUsageTokens("codex", codex)
	if _, known := got["input"]; known || got["cacheWrite"] != 3 || gap == "" {
		t.Fatal("unknown overlap guessed", got, gap)
	}
	claude := map[string]int64{"input_tokens": 3, "cache_read_input_tokens": 80, "cache_creation_input_tokens": 12, "output_tokens": 7, "output_tokens_details.thinking_tokens": 2}
	got, gap = NormalizeUsageTokens("claude", claude)
	if gap != "" || len(got) != 5 || got["input"] != 3 || got["output"] != 5 || got["reasoning"] != 2 {
		t.Fatal(got, gap)
	}
}
