package server

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenIdentity(t *testing.T) {
	token := strings.Repeat("a", 48)
	identity, err := TokenIdentity(token, api.Caller{Node: "workspace", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"", "Bearer wrong", "Basic " + token, "Bearer " + token} {
		r := httptest.NewRequest("GET", "/v1/whoami", nil)
		r.Header.Set("Authorization", header)
		r.Header.Set("X-Forwarded-User", "owner")
		who, err := identity(r)
		if header == "Bearer "+token {
			if err != nil || who.User != "owner" {
				t.Fatalf("valid token: %v %v", who, err)
			}
		} else if err == nil {
			t.Fatalf("accepted %q", header)
		}
	}
	if _, err := TokenIdentity("", api.Caller{}); err == nil {
		t.Fatal("accepted empty secret")
	}
}
