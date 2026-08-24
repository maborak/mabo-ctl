package service

import (
	"fmt"
	"strings"
	"text/template"
)

// expander expands the {{.Port}} templates config held raw. It is constructed
// only after EVERY port has resolved, because a service may reference another
// service's port and half-resolved data would expand to a lie.
type expander struct {
	// names lists every declared service in declaration order, for error
	// messages.
	names []string
	// ports maps a service name to its resolved port. A portless service is
	// present with the value 0.
	ports map[string]int
}

// newExpander builds an expander over the declared service names and their
// resolved ports.
func newExpander(names []string, ports map[string]int) *expander {
	return &expander{names: names, ports: ports}
}

// expand renders one template. svc is the service the text belongs to and
// supplies the meaning of a bare {{.Port}}; what names the field for error
// messages ("health", "cmd[3]", `env["API_BASE"]`).
//
// Text with no action is returned unchanged. Templates run with
// Option("missingkey=error"), so an unresolvable reference is an error rather
// than the string "<no value>" silently reaching a command line. It returns an
// error when the text does not parse, references an unknown service, or
// references the port of a service that declares none.
func (e *expander) expand(svc, what, text string) (string, error) {
	if !strings.Contains(text, "{{") {
		return text, nil
	}
	tpl, err := template.New(svc + " " + what).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("service %q: %s %q: invalid template: %w", svc, what, text, err)
	}
	var b strings.Builder
	if err := tpl.Execute(&b, &portData{self: svc, exp: e}); err != nil {
		return "", fmt.Errorf("service %q: %s %q: %w", svc, what, text, err)
	}
	return b.String(), nil
}

// portData is the data a template executes against. Its only member is the Port
// method, which is what makes both {{.Port}} and {{.Port "other"}} legal: a
// variadic method is called with zero arguments in the first form and one in the
// second.
type portData struct {
	self string
	exp  *expander
}

// Port returns this service's resolved port when called as {{.Port}}, or
// another service's when called as {{.Port "name"}}.
//
// It returns an error — which aborts the template and surfaces to the caller —
// when more than one name is given, when the named service is not declared (the
// message lists the ones that are), or when the named service has no port, since
// expanding a URL to "http://localhost:0/" would produce a health check that can
// only ever fail.
func (d *portData) Port(name ...string) (int, error) {
	switch len(name) {
	case 0:
		return d.lookup(d.self, true)
	case 1:
		return d.lookup(name[0], name[0] == d.self)
	default:
		return 0, fmt.Errorf(`{{.Port}} takes at most one service name, got %d`, len(name))
	}
}

// lookup resolves one service's port, reporting an unknown name or a portless
// service as an error. own distinguishes the two phrasings.
func (d *portData) lookup(name string, own bool) (int, error) {
	port, ok := d.exp.ports[name]
	if !ok {
		return 0, fmt.Errorf("unknown service %q; declared services are: %s",
			name, strings.Join(d.exp.names, ", "))
	}
	if port <= 0 {
		if own {
			return 0, fmt.Errorf("service %q declares no port, so {{.Port}} has nothing to expand to; "+
				"give it a port or drop the reference", name)
		}
		return 0, fmt.Errorf("service %q declares no port, so {{.Port %q}} has nothing to expand to",
			name, name)
	}
	return port, nil
}
