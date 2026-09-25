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
