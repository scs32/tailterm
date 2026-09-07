package adapters

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeHooksIsValidJSON(t *testing.T) {
	var v struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal([]byte(ClaudeHooks("/usr/local/bin/tt")), &v); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "Notification"} {
		if len(v.Hooks[ev]) != 1 || !strings.HasPrefix(v.Hooks[ev][0].Hooks[0].Command, "/usr/local/bin/tt hook ") {
			t.Errorf("%s hook missing or wrong: %+v", ev, v.Hooks[ev])
		}
	}
	if !strings.Contains(CodexConfig("tt"), `notify = ["tt", "hook", "codex"]`) {
		t.Error("codex config wrong")
	}
}
