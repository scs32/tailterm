package store

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Queue scopes are logical repository-relative paths. A missing declaration is
// deliberately global: legacy entries must never be assumed independent.
func canonicalQueueOwnership(paths []string) ([]string, error) {
	if len(paths) > 256 {
		return nil, api.ErrInvalid
	}
	out := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		if !utf8.ValidString(p) || p == "" || len(p) > 1024 || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") || strings.Contains(p, "//") || strings.HasSuffix(p, "/") || strings.Contains(p, ":") {
			return nil, fmt.Errorf("%w: invalid ownership path", api.ErrInvalid)
		}
		for _, part := range strings.Split(p, "/") {
			if part == "." || part == ".." || part == "" {
				return nil, fmt.Errorf("%w: ownership path traversal or alias", api.ErrInvalid)
			}
		}
		if path.Clean(p) != p {
			return nil, fmt.Errorf("%w: ownership path is not canonical", api.ErrInvalid)
		}
		key := strings.ToLower(p)
		if seen[key] {
			return nil, fmt.Errorf("%w: duplicate or case alias ownership path", api.ErrInvalid)
		}
		seen[key] = true
		out = append(out, p)
	}
	return out, nil
}

func queueScopesConflict(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, left := range a {
		left = strings.ToLower(left)
		for _, right := range b {
			right = strings.ToLower(right)
			if left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/") {
				return true
			}
		}
	}
	return false
}

func queueEntryConflicts(a, b api.TeamQueueEntry) bool {
	return a.Repository == "" || b.Repository == "" || len(a.Ownership) == 0 || len(b.Ownership) == 0 || (a.Repository == b.Repository && queueScopesConflict(a.Ownership, b.Ownership))
}

// queueHoldsSQL is the SQL form of queueEntryHoldsResources.
const queueHoldsSQL = `(state IN ('launching','running','failed') AND released_at='')`

// queueEntryHoldsResources reports whether an entry holds a team slot, a
// handler lease and its ownership. A released entry holds none of them, even
// while an owner-integrated team is still live for post-release checks.
func queueEntryHoldsResources(e api.TeamQueueEntry) bool {
	return (e.State == "launching" || e.State == "running" || e.State == "failed") && e.ReleasedAt == ""
}

// queueClosable reports whether the runner may close and finish an entry's
// team: a running entry, or a failed one the owner recorded as integrated.
func queueClosable(e api.TeamQueueEntry) bool {
	return e.State == "running" || (e.State == "failed" && e.OwnerIntegration != nil)
}

// queueNarrows reports whether every path in next lies under a path in
// current, so a new declaration can only give way.
func queueNarrows(next, current []string) bool {
	if len(current) == 0 {
		return true // an unscoped or serial entry held everything
	}
	for _, p := range next {
		p = strings.ToLower(p)
		inside := false
		for _, held := range current {
			held = strings.ToLower(held)
			if p == held || strings.HasPrefix(p, held+"/") {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}
