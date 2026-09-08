package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

func TestProfileRoutesRequireSeparateCredentials(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	admin, key := strings.Repeat("x", 40), strings.Repeat("a", 64)
	identity, _ := TokenIdentity(admin, api.Caller{Node: "owner"})
	s := New(st, identity)
	body := []byte(`{"revision":1,"envelope":{"format":"tailterm-profile","version":1,"iv":"AAAAAAAAAAAAAAAA","ciphertext":"AAAAAAAAAAAAAAAAAAAAAA=="}}`)
	request := func(method, path, auth, profileKey string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Authorization", auth)
		r.Header.Set("X-Profile-Key", profileKey)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/v1/profiles", "", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "instanceId") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", "/v1/profiles/alice", "Profile "+key, key, body); w.Code != 403 {
		t.Fatal("unauthorized enrollment", w.Code)
	}
	if w := request("POST", "/v1/profiles/alice", "Bearer "+admin, key, body); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, method := range []string{"GET", "PUT"} {
		if w := request(method, "/v1/profiles/alice", "Bearer "+admin, "", body); w.Code != 403 {
			t.Fatal("agent token reached profile", method, w.Code)
		}
	}
	if w := request("GET", "/v1/profiles/alice", "Profile "+key, "", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w := request("PUT", "/v1/profiles/alice", "Profile "+key, "", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var p store.Profile
	json.Unmarshal(w.Body.Bytes(), &p)
	if p.Revision != 2 {
		t.Fatal(p)
	}
	if w := request("PUT", "/v1/profiles/alice", "Profile "+key, "", body); w.Code != http.StatusConflict {
		t.Fatal("lost update", w.Code)
	}
	if w := request("GET", "/v1/profiles/bob", "Profile "+key, "", nil); w.Code != 403 {
		t.Fatal("enumeration", w.Code)
	}
}
