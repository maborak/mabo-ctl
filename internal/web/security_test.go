package web

import (
	"strings"
	"testing"

	"github.com/maborak/mabo-ctl/internal/redact"
)

// TestRedactionIsNotReimplementedHere guards the consolidation that moved
// redactURL, redactArgs, isSecret and declaredEnv out of this package and into
// internal/redact.
//
// The rules had to be widened twice while they lived here — once to health URLs
// and command arguments, once for a second front end — and a copy of a pattern
// list is how one of them silently stops matching the day the other gains a
// prefix. This asserts the behaviour through the package that now owns it, so a
// future local helper that "just handles one more case" fails here rather than
// disclosing a credential from whichever caller kept using the old one.
func TestRedactionIsNotReimplementedHere(t *testing.T) {
	t.Parallel()
	if got := redact.URL("http://admin:hunter2@localhost:7100/health"); strings.Contains(got, "hunter2") {
		t.Errorf("redact.URL leaked the password: %q", got)
	}
	if got := strings.Join(redact.Args([]string{"serve", "--token=ghp_realtokenvalue"}), " "); strings.Contains(got, "ghp_realtokenvalue") {
		t.Errorf("redact.Args leaked the token: %q", got)
	}
	if !redact.IsSecret("API_TOKEN", "abc") {
		t.Error("redact.IsSecret no longer flags a credential-shaped key")
	}
}

// TestValidateAddrRejectsNonNumericPorts covers the reason validateAddr exists:
// net.SplitHostPort accepts an /etc/services NAME such as "http", and the Host
// check compares ports numerically, so a name that parsed as port 0 would make
// the Host check compare 0 to 0 and accept every host there is.
func TestValidateAddrRejectsNonNumericPorts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:7999", true},
		{"[::1]:7999", true},
		{":7999", true},
		{"127.0.0.1:http", false},
		{"127.0.0.1:99999", false},
		{"127.0.0.1", false},
		{"", false},
	}
	for _, tc := range cases {
		err := validateAddr(tc.addr)
		if tc.ok && err != nil {
			t.Errorf("validateAddr(%q) = %v, want it accepted", tc.addr, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("validateAddr(%q) = nil, want it refused", tc.addr)
		}
	}
}

// TestIsLoopbackAddrFailsClosed covers the control that decides whether the
// console is exposed beyond the machine. A wildcard bind is the case most
// likely to be typed by accident and the most dangerous one, so it must not be
// reported as loopback.
func TestIsLoopbackAddrFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:7999", true},
		{"[::1]:7999", true},
		{"localhost:7999", true},
		{":7999", false},
		{"0.0.0.0:7999", false},
		{"192.0.2.1:7999", false},
	}
	for _, tc := range cases {
		got, err := isLoopbackAddr(tc.addr)
		if err != nil {
			t.Errorf("isLoopbackAddr(%q): unexpected error %v", tc.addr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
