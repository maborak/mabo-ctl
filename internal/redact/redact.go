// Package redact withholds credential-shaped values from anything mabo-ctl shows
// a reader: the web console's JSON routes, `mabo-ctl config`, and whatever comes
// next.
//
// It is pure — no files, no processes, no global state — and it is the ONLY
// place these rules live. That is the whole reason the package exists. The
// rules started in internal/web, applied to declared environment values alone,
// and had to be widened twice: once to health URLs and command arguments, which
// were being copied out verbatim beside the variables they duplicated, and
// again when a second front end needed the same answers. A second copy of a
// pattern list is how one of them silently stops matching the day someone adds
// a prefix to the other, so callers import this and never restate a rule.
//
// The bias is deliberate: over-redaction costs a developer the value of a
// variable they can read in mabo-ctl.yaml themselves, while under-redaction puts
// a live key on a page or in a terminal recording. Keys are ALWAYS rendered —
// "this service sets DATABASE_PASSWORD" is exactly what a developer needs to
// know, and the value is exactly what they do not.
package redact

import (
	"regexp"
	"sort"
	"strings"
)

// Mark is what replaces a credential anywhere it would otherwise be rendered.
const Mark = "[redacted]"

// secretKeyPattern matches an environment variable name whose VALUE must never
// be rendered. It is applied to the key only, case-insensitively.
var secretKeyPattern = regexp.MustCompile(`(?i)(token|secret|key|password|passwd|credential|auth)`)

// credentialURLPattern matches a URL carrying userinfo with a password, such as
// postgres://app:hunter2@db/app. The key of such a variable is usually
// DATABASE_URL, which no key pattern would ever flag, and the value is a live
// credential.
var credentialURLPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*://[^/?#@\s]+:[^/?#@\s]+@`)

// userinfoPattern matches ANY userinfo in a URL, with or without a colon.
//
// credentialURLPattern deliberately requires user:pass, because that is the
// shape that proves a whole value is a credential. But a bare-token userinfo —
// https://ghp_xxx@github.com — is the standard form for git clone URLs, npm and
// pip index URLs, and plenty of internal health endpoints, and it carries a live
// secret with no colon anywhere. Redacting a URL uses this wider pattern; the
// is-this-value-a-credential test keeps the narrower one.
var userinfoPattern = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.\-]*://)([^/?#@\s]+)@`)

// secretValuePrefixes are value shapes that are credentials whatever they are
// called. The list is deliberately short: over-redaction only costs a developer
// the value of a variable they can read in mabo-ctl.yaml, while under-redaction
// puts a live key on a web page.
var secretValuePrefixes = []string{
	"sk-", "sk_", "rk_", "ghp_", "gho_", "ghu_", "ghs_", "github_pat_",
	"xox", "AKIA", "ASIA", "eyJ", "-----BEGIN",
}

// Var is one declared environment variable as it is rendered.
type Var struct {
	// Key is the variable name, always rendered.
	Key string `json:"key"`
	// Value is the declared value, or [Mark] when Redacted is set.
	Value string `json:"value"`
	// Redacted reports that the real value was withheld, so a reader can say so
	// rather than showing "[redacted]" as if the service set it to that.
	Redacted bool `json:"redacted"`
}

// IsSecret reports whether a variable's value must be withheld: its key names a
// credential, or its value is shaped like one.
func IsSecret(key, value string) bool {
	if secretKeyPattern.MatchString(key) {
		return true
	}
	if value == "" {
		return false
	}
	if credentialURLPattern.MatchString(value) {
		return true
	}
	for _, p := range secretValuePrefixes {
		if strings.HasPrefix(value, p) {
			return true
		}
	}
	return false
}

// Env renders a service's DECLARED environment, sorted by key.
//
// The argument is config.Spec.Env and never service.Instance.Env. The
// distinction is the whole point: Spec.Env is the handful of variables written
// in mabo-ctl.yaml, while Instance.Env is the entire environment mabo-ctl was
// started with, forwarded to the child. Rendering the latter would publish the
// user's real AWS keys, GitHub tokens and shell history variables, which is why
// this function cannot be handed one.
//
// The result is never nil, so a JSON consumer sees [] rather than null.
func Env(declared map[string]string) []Var {
	out := make([]Var, 0, len(declared))
	keys := make([]string, 0, len(declared))
	for k := range declared {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := declared[k]
		if IsSecret(k, v) {
			out = append(out, Var{Key: k, Value: Mark, Redacted: true})
			continue
		}
		out = append(out, Var{Key: k, Value: v})
	}
	return out
}

// URL strips credentials from a URL that will be shown to a reader.
//
// It exists because redaction was originally applied only to declared
// environment values, while `health` and `cmd` were copied verbatim into the
// console's JSON. A health URL of the form
// http://admin:hunter2@host/health?api_key=sk-live-… therefore disclosed both
// the password and the key in full, while an environment variable holding the
// identical string was redacted — the control existed and simply was not wired
// to these fields.
//
// Two forms are handled: userinfo before the host, and any query parameter
// whose NAME looks secret by the same rule used for environment keys.
func URL(raw string) string {
	if raw == "" {
		return raw
	}
	out := raw
	if m := userinfoPattern.FindStringSubmatch(out); m != nil {
		scheme, userinfo := m[1], m[2]
		rest := out[len(m[0]):]
		if user, _, hasPass := strings.Cut(userinfo, ":"); hasPass {
			// user:pass — keep the username, which is useful and rarely secret.
			out = scheme + user + ":" + Mark + "@" + rest
		} else {
			// A bare userinfo token IS the credential; there is no username to
			// keep, and showing it would be showing the secret.
			out = scheme + Mark + "@" + rest
		}
	}
	q := strings.IndexAny(out, "?")
	if q < 0 {
		return out
	}
	head, tail := out[:q+1], out[q+1:]
	parts := strings.Split(tail, "&")
	for i, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		if !ok || v == "" {
			continue
		}
		if IsSecret(k, v) {
			parts[i] = k + "=" + Mark
		}
	}
	return head + strings.Join(parts, "&")
}

// Args strips credentials from an argv vector before it is rendered.
//
// A command is genuinely useful to see — it is half of what the operator asked
// the console and `mabo-ctl config` to show — but arguments carry tokens and
// connection strings just as often as environment variables do. Three shapes
// are covered: --flag=VALUE where the flag name looks secret, a bare value that
// looks like a credential (an sk-/ghp-style prefix, or a URL with userinfo),
// and the value FOLLOWING a secret-looking flag written as two arguments.
//
// The input is never modified; the result is a new slice of the same length.
func Args(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	redactNext := false
	for i, a := range out {
		if redactNext {
			out[i] = Mark
			redactNext = false
			continue
		}
		// KEY=VALUE, with or without a leading dash. The dash test used to gate
		// this, which meant `env DATABASE_URL=postgres://app:pw@db cmd` — the
		// most ordinary way a credential reaches an argv — fell through to
		// URL(), a no-op there because the string starts with "DATABASE_URL="
		// rather than a scheme.
		flag, val, ok := strings.Cut(a, "=")
		if ok && flag != "" {
			if IsSecret(strings.TrimLeft(flag, "-"), val) {
				out[i] = flag + "=" + Mark
			} else {
				out[i] = flag + "=" + URL(val)
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			// A lone secret-looking flag takes its value as the next argument.
			if secretKeyPattern.MatchString(strings.TrimLeft(a, "-")) {
				redactNext = true
			}
			continue
		}
		if IsSecret("", a) {
			out[i] = Mark
			continue
		}
		out[i] = URL(a)
	}
	return out
}

// YAML redacts a mabo-ctl.yaml that is about to be displayed, leaving every byte
// of its STRUCTURE alone: indentation, key order, comments, blank lines and
// quoting all survive, and only values that [IsSecret] flags are replaced.
//
// It is not a YAML parser and deliberately is not one. Re-serialising the file
// through a decoder would silently rewrite the operator's comments, anchors and
// quoting style into whatever the encoder prefers, and the point of showing the
// raw file is that it is the file — so this walks lines and reuses exactly the
// rules [Env] and [Args] apply, rather than inventing a second set that could
// drift from them.
//
// A mapping value is judged by [IsSecret] on the key AND the value, matching
// what the console does with declared environment variables. Anything that is
// not a mapping line — a flow sequence, a bare scalar, a list entry — is split
// on spaces and run through [Args], which is how a `--token=…` inside a `cmd:`
// vector is caught. Splitting and rejoining on a single space is lossless, so
// an untouched line comes back byte-identical.
func YAML(text string) string {
	if text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))

	// Block scalars make this stateful. `api_key: |` puts the secret on the
	// FOLLOWING lines, so a line-at-a-time pass redacted the harmless `|`
	// indicator and printed the secret underneath it untouched — hitting the one
	// character that was safe and missing the one thing that was not. When a
	// secret key opens a block, every line indented deeper than that key is part
	// of its value and is dropped.
	blockIndent := -1
	for _, l := range lines {
		if blockIndent >= 0 {
			if strings.TrimSpace(l) == "" || lineIndent(l) > blockIndent {
				continue
			}
			blockIndent = -1
		}
		red, opened := yamlLine(l)
		out = append(out, red)
		if opened >= 0 {
			blockIndent = opened
		}
	}
	return strings.Join(out, "\n")
}

// lineIndent counts the leading whitespace columns of a line.
func lineIndent(line string) int {
	n := 0
	for n < len(line) && (line[n] == ' ' || line[n] == '\t') {
		n++
	}
	return n
}

// blockScalarPattern matches a YAML block-scalar header: | or >, optionally
// with a chomping indicator and an explicit indent.
var blockScalarPattern = regexp.MustCompile(`^[|>][-+]?[0-9]*$`)

// flowMappingPattern matches an inline {k: v, k2: v2} mapping.
var flowMappingPattern = regexp.MustCompile(`^\{.*\}$`)

// yamlLine redacts one line of a YAML document, preserving its structure.
//
// It returns the rewritten line and, when that line opens a block scalar whose
// key is secret, the indent of the key so [YAML] can drop the block body.
// A non-opening line returns -1.
func yamlLine(line string) (string, int) {
	prefix, body := yamlPrefix(line)
	if body == "" || strings.HasPrefix(body, "#") {
		return line, -1
	}
	key, rest, ok := strings.Cut(body, ":")
	// Any whitespace separates a key from its value, not only a space: a tab
	// is legal YAML indentation between them and used to bypass this entirely.
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return prefix + yamlScalar(body), -1
	}

	value := strings.TrimLeft(rest, " \t")
	gap := rest[:len(rest)-len(value)]
	if value == "" {
		return line, -1
	}
	if blockScalarPattern.MatchString(value) {
		// The value is on the lines below. Say so here and let YAML drop them.
		if IsSecret(key, "") {
			return prefix + key + ":" + gap + Mark, lineIndent(line)
		}
		return line, -1
	}
	if IsSecret(key, value) {
		return prefix + key + ":" + gap + Mark, -1
	}
	if flowMappingPattern.MatchString(value) {
		return prefix + key + ":" + gap + yamlFlowMapping(value), -1
	}
	return prefix + key + ":" + gap + yamlScalar(value), -1
}

// yamlFlowMapping redacts the pairs of an inline {k: v, k2: v2} mapping, which
// a line-oriented pass would otherwise treat as one opaque scalar.
func yamlFlowMapping(value string) string {
	inner := strings.TrimSuffix(strings.TrimPrefix(value, "{"), "}")
	parts := strings.Split(inner, ",")
	for i, pair := range parts {
		k, v, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		kt, vt := strings.TrimSpace(k), strings.TrimSpace(v)
		if vt == "" {
			continue
		}
		lead := pair[:len(pair)-len(strings.TrimLeft(pair, " \t"))]
		if IsSecret(kt, vt) {
			parts[i] = lead + kt + ": " + Mark
		} else {
			parts[i] = lead + kt + ": " + yamlScalar(vt)
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// yamlPrefix splits a line into the structural prefix that carries no value —
// leading whitespace and any number of "- " sequence markers — and the rest.
func yamlPrefix(line string) (prefix, body string) {
	i := 0
	for {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if strings.HasPrefix(line[i:], "- ") {
			i += 2
			continue
		}
		return line[:i], line[i:]
	}
}

// yamlScalar redacts the value part of a line by treating it as an argv vector,
// so a credential inside a `cmd:` flow sequence is caught by the same rule that
// catches it in the resolved command.
//
// Splitting on a single space and rejoining with one is exactly reversible, so
// a value with nothing to redact is returned unchanged, spacing included. Each
// token is unwrapped from the punctuation a flow sequence or a quoted scalar
// puts around it before [Args] sees it, and rewrapped afterwards.
func yamlScalar(value string) string {
	tokens := strings.Split(value, " ")
	heads := make([]string, len(tokens))
	cores := make([]string, len(tokens))
	tails := make([]string, len(tokens))
	for i, t := range tokens {
		h := 0
		for h < len(t) && strings.ContainsRune(`[{"'`, rune(t[h])) {
			h++
		}
		e := len(t)
		for e > h && strings.ContainsRune(`]},"'`, rune(t[e-1])) {
			e--
		}
		heads[i], cores[i], tails[i] = t[:h], t[h:e], t[e:]
	}
	safe := Args(cores)
	for i := range tokens {
		tokens[i] = heads[i] + safe[i] + tails[i]
	}
	return strings.Join(tokens, " ")
}
