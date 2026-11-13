//go:build unix

package supervisor

import (
	"slices"
	"testing"
)

func TestParseGroupMembers(t *testing.T) {
	t.Parallel()
	out := "    1     1\n  412   400\n  400   400\nbogus line\n  401   400 extra\n   77  4000\n"
	if got := parseGroupMembers(out, 400); !slices.Equal(got, []int{412, 400}) {
		t.Errorf("members of 400 = %v, want [412 400]", got)
	}
	if got := parseGroupMembers(out, 1); len(got) != 0 {
		t.Errorf("init must never be listed as a member: %v", got)
	}
}

func TestParseCwds(t *testing.T) {
	t.Parallel()
	out := "p412\nfcwd\nn/Users/dev/app\np400\nfcwd\nn/private/var/x y\n"
	got := parseCwds(out)
	if got[412] != "/Users/dev/app" || got[400] != "/private/var/x y" || len(got) != 2 {
		t.Errorf("parseCwds = %v", got)
	}
}

func TestWithin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path, root string
		want       bool
	}{
		{"/repo", "/repo", true},
		{"/repo/api", "/repo", true},
		{"/repository", "/repo", false},
		{"/", "/repo", false},
		{"/repo/..foo", "/repo", true},
	} {
		if got := within(tc.path, tc.root); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
		}
	}
}
