package store

import "testing"

func TestQueueOwnershipCanonicalAndOverlap(t *testing.T) {
	for _, bad := range []string{"", "/src", "../src", "src/../go", "./src", "src//go", "src/", "src\\go", "src:go", "src/./go"} {
		if _, err := canonicalQueueOwnership([]string{bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := canonicalQueueOwnership([]string{"src/Foo", "src/foo"}); err == nil {
		t.Fatal("accepted case alias")
	}
	for _, tc := range []struct {
		a, b []string
		want bool
	}{
		{[]string{"src/a.go"}, []string{"src/a.go"}, true},
		{[]string{"src"}, []string{"src/a.go"}, true},
		{[]string{"src/a"}, []string{"src/ab"}, false},
		{[]string{"src/a.go"}, []string{"src/b.go"}, false},
		{nil, []string{"src/a.go"}, true},
	} {
		if got := queueScopesConflict(tc.a, tc.b); got != tc.want {
			t.Errorf("overlap %v %v = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}
