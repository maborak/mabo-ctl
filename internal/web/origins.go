package web

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// ErrBadOrigin reports an origin that mabo-ctl will not trust.
var ErrBadOrigin = errors.New("web: not a usable origin")

// ErrOriginLockout reports an origin-list change that would refuse the very
// request making it.
var ErrOriginLockout = errors.New("web: that change would lock this browser out")

// maxTrustedOrigins bounds the list. It is not a security boundary — the token
// is — but an unbounded list editable over HTTP is a memory growth path, and
// nobody legitimately tunnels a dev console through fifty hostnames.
const maxTrustedOrigins = 32

// NormalizeOrigin validates raw and returns it in the canonical form a browser
// puts in the Origin header: scheme://host[:port], lower-cased, no trailing
// slash, no path, no credentials.
//
// Comparing origins as raw strings is how allowlists get bypassed — "HTTPS://X"
// and "https://x/" are the same origin and would miss an exact-match test — so
// every entry goes through here on the way in and every comparison is made
// against the canonical form.
//
// Plaintext http is refused for anything but a loopback host. An http origin on
// a public name is spoofable by anyone on the path, so trusting one would hand
// the trust to the network rather than to the name.
func NormalizeOrigin(raw string) (string, error) {
	return normalizeOrigin(raw, false)
}

// NormalizeOriginAllowingAny is [NormalizeOrigin] plus the bare wildcard "*".
//
// It is separate, and the flag is threaded from Options.Force, because "*" is
// the one value that stops the list being a list. Every other entry names
// something; "*" means "whatever page is asking", which cannot be audited later
// by reading it. It is accepted only where the operator has already said
// --i-know-this-is-dangerous.
func NormalizeOriginAllowingAny(raw string) (string, error) {
	return normalizeOrigin(raw, true)
}

// AnyOrigin is the bare wildcard, matching every origin.
const AnyOrigin = "*"

func normalizeOrigin(raw string, allowAny bool) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%w: empty", ErrBadOrigin)
	}
	if s == AnyOrigin {
		if !allowAny {
			return "", fmt.Errorf(
				"%w: %q accepts every origin, including a page you did not open. "+
					"Name the host instead (https://dev.tunnel.example), or a subdomain pattern "+
					"(https://*.tunnel.example). Pass --i-know-this-is-dangerous to mean it",
				ErrBadOrigin, s)
		}
		return AnyOrigin, nil
	}
	// "null" is what a sandboxed iframe and a file:// page send, so it names a
	// class of contexts rather than a site and can never be trusted.
	if strings.EqualFold(s, "null") {
		return "", fmt.Errorf("%w: %q names a class of contexts, not a site", ErrBadOrigin, s)
	}

	// A subdomain pattern is parsed by standing a placeholder label in for the
	// "*", because net/url will not parse a host containing one.
	wildcard := false
	if i := strings.Index(s, "://*."); i >= 0 {
		wildcard = true
		s = s[:i+3] + s[i+5:] // drop the "*." and validate what remains
	} else if strings.Contains(s, "*") {
		return "", fmt.Errorf(
			"%w: %q — a wildcard is only meaningful as a whole subdomain, as in https://*.tunnel.example",
			ErrBadOrigin, s)
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %v", ErrBadOrigin, s, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: %q must start with https:// (or http:// for a loopback host)", ErrBadOrigin, s)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: %q carries credentials; an origin is scheme, host and port only", ErrBadOrigin, s)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: %q names no host", ErrBadOrigin, s)
	}
	if p := strings.Trim(u.Path, "/"); p != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf(
			"%w: %q has a path or query; an origin is scheme://host[:port] and nothing more", ErrBadOrigin, s)
	}

	host := strings.ToLower(u.Host)
	if scheme == "http" && !isLoopbackHostPort(host) {
		return "", fmt.Errorf(
			"%w: %q is plaintext http on a public name — anyone on the network could claim it; use https://",
			ErrBadOrigin, s)
	}
	if !wildcard {
		return scheme + "://" + host, nil
	}

	// A pattern must still name something. "*.com" or "*.local" would hand the
	// trust to a registry rather than to a host, so the suffix has to carry at
	// least two labels of its own.
	bare := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		bare = h
	}
	if strings.Count(bare, ".") < 1 || bare == "" {
		return "", fmt.Errorf(
			"%w: %q is too broad — a subdomain pattern needs a domain under it, as in https://*.tunnel.example",
			ErrBadOrigin, raw)
	}
	return scheme + "://*." + host, nil
}

// originMatches reports whether canonical origin o is covered by entry, which is
// either an exact origin, a "scheme://*.suffix" pattern, or [AnyOrigin].
//
// The suffix test is a plain string suffix on scheme://host[:port], which works
// because the port sits at the END of both sides: a pattern with a port matches
// only that port, and one without matches only the default. The leading "." is
// part of the comparison, so "*.tunnel.example" covers "a.tunnel.example" and
// never "eviltunnel.example".
func originMatches(entry, o string) bool {
	if entry == AnyOrigin {
		return true
	}
	if entry == o {
		return true
	}
	i := strings.Index(entry, "://*.")
	if i < 0 {
		return false
	}
	scheme, suffix := entry[:i+3], entry[i+5:]
	if !strings.HasPrefix(o, scheme) {
		return false
	}
	return strings.HasSuffix(o, "."+suffix)
}

// isLoopbackHostPort reports whether a host[:port] names this machine.
func isLoopbackHostPort(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// originSet is the mutable allowlist of origins the console trusts, on top of
// the loopback address it is bound to.
//
// It is mutable because the address a browser reaches the console on is not
// always known when the process starts: a tunnel is set up afterwards, and
// restarting mabo-ctl to add its hostname would stop every supervised service.
type originSet struct {
	mu   sync.RWMutex
	list []string // canonical, sorted, deduplicated
	// allowAny mirrors Options.Force. It is the only thing that lets the bare
	// wildcard into the list, and it is stored here so the runtime editor obeys
	// exactly the same rule the command line did.
	allowAny bool
}

// normalize applies this set's rules to one entry.
func (s *originSet) normalize(raw string) (string, error) {
	if s.allowAny {
		return NormalizeOriginAllowingAny(raw)
	}
	return NormalizeOrigin(raw)
}

// add seeds the set, reporting the first value it will not take.
func (s *originSet) add(raws ...string) error {
	for _, raw := range raws {
		o, err := s.normalize(raw)
		if err != nil {
			return err
		}
		s.mu.Lock()
		if !contains(s.list, o) {
			s.list = append(s.list, o)
			sort.Strings(s.list)
		}
		n := len(s.list)
		s.mu.Unlock()
		if n > maxTrustedOrigins {
			return fmt.Errorf("%w: more than %d trusted origins", ErrBadOrigin, maxTrustedOrigins)
		}
	}
	return nil
}

// replace swaps the whole list, after normalising every entry. It is all or
// nothing: a list where one entry is rejected leaves the old list untouched,
// so a bad edit cannot half-apply.
func (s *originSet) replace(raws []string) ([]string, error) {
	if len(raws) > maxTrustedOrigins {
		return nil, fmt.Errorf("%w: %d origins exceeds the limit of %d", ErrBadOrigin, len(raws), maxTrustedOrigins)
	}
	next := make([]string, 0, len(raws))
	for _, raw := range raws {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		o, err := s.normalize(raw)
		if err != nil {
			return nil, err
		}
		if !contains(next, o) {
			next = append(next, o)
		}
	}
	sort.Strings(next)

	s.mu.Lock()
	s.list = next
	s.mu.Unlock()
	return append([]string(nil), next...), nil
}

// snapshot returns a copy of the list.
func (s *originSet) snapshot() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.list...)
}

// has reports whether canonical origin o is covered by any entry — exactly, by
// a subdomain pattern, or by the bare wildcard.
func (s *originSet) has(o string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.list {
		if originMatches(e, o) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// TrustedOrigins returns the origins the console currently accepts IN ADDITION
// to the loopback address it is bound to.
func (s *Server) TrustedOrigins() []string { return s.trusted.snapshot() }

// setTrustedOrigins replaces the list, refusing a change that would lock the
// caller out.
//
// requestOrigin is the Origin of the request asking for the change, or "" for a
// caller that sent none. That is the guard: an operator editing the list from
// the console page reachable at https://tunnel.example must not be able to
// delete that entry and lose the buttons they would need to put it back. The
// loopback origins are always allowed and are never in this list, so a browser
// on 127.0.0.1 can always recover a console that was locked out this way — the
// refusal below exists so the operator never has to.
func (s *Server) setTrustedOrigins(next []string, requestOrigin string) ([]string, error) {
	if requestOrigin != "" {
		canon, err := NormalizeOrigin(requestOrigin)
		// An unparseable Origin cannot be checked for lockout. It also cannot
		// have passed the guard, so this is unreachable in practice; refusing
		// rather than assuming is the right way to be wrong here.
		if err != nil {
			return nil, fmt.Errorf("%w: this request's own Origin is unusable: %v", ErrOriginLockout, err)
		}
		// Only origins that need the list are at risk. A browser on the bound
		// loopback address keeps working no matter what this list says.
		if !s.allowedOriginImplicit(canon) {
			ok := false
			for _, raw := range next {
				// Coverage, not equality: a caller on a.tunnel.example is kept
				// safe by a surviving https://*.tunnel.example entry.
				if c, err := s.trusted.normalize(raw); err == nil && originMatches(c, canon) {
					ok = true
					break
				}
			}
			if !ok {
				return nil, fmt.Errorf(
					"%w: you are using the console from %s, and that origin is not in the new list. "+
						"Keep it, or make the change from http://%s first",
					ErrOriginLockout, canon, s.Addr())
			}
		}
	}
	return s.trusted.replace(next)
}
