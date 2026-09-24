package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
)

// TokenIdentity provides a single workspace identity on an existing private network.
// No proxy headers or unauthenticated source addresses are trusted.
func TokenIdentity(token string, caller api.Caller) (Identity, error) {
	if len(token) < 32 {
		return nil, errors.New("hub token must contain at least 32 characters")
	}
	expected := sha256.Sum256([]byte("Bearer " + token))
	return func(r *http.Request) (api.Caller, error) {
		actual := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
			return api.Caller{}, errors.New("invalid hub token")
		}
		return caller, nil
	}, nil
}

// TokenIdentities resolves each of several distinct bearer tokens to its own
// caller, for example the workspace owner and the Discord bridge.
func TokenIdentities(tokens map[string]api.Caller) (Identity, error) {
	type entry struct {
		digest [32]byte
		caller api.Caller
	}
	var entries []entry
	for token, caller := range tokens {
		if len(token) < 32 {
			return nil, errors.New("hub token must contain at least 32 characters")
		}
		entries = append(entries, entry{sha256.Sum256([]byte("Bearer " + token)), caller})
	}
	if len(entries) == 0 {
		return nil, errors.New("no hub tokens")
	}
	return func(r *http.Request) (api.Caller, error) {
		actual := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		for _, e := range entries {
			if subtle.ConstantTimeCompare(actual[:], e.digest[:]) == 1 {
				return e.caller, nil
			}
		}
		return api.Caller{}, errors.New("invalid hub token")
	}, nil
}
