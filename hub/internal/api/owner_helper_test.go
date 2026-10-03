package api

import (
	"errors"
	"testing"
)

func TestOwnerHelperRuntimeValidation(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "", "unsupported", "Codex"} {
		req := RegisterOwnerHelperRequest{Host: "owner-host", Session: "owner", Runtime: runtime, Cwd: "/work"}
		err := ValidateRegisterOwnerHelper(&req)
		if runtime == "claude" || runtime == "codex" {
			if err != nil || req.Name != DefaultOwnerHelperName {
				t.Fatalf("%s: %+v %v", runtime, req, err)
			}
		} else if !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsupported runtime %q: %v", runtime, err)
		}
	}
}
