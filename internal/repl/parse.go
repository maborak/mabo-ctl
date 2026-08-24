package repl

import (
	"fmt"
	"strings"
)

// tokenize splits one typed line into the argv vector the command tree
// receives.
//
// It understands exactly what is needed to type a mabo-ctl command by hand:
// whitespace separates words, single quotes take their contents literally,
// double quotes take theirs literally except for a backslash before a double
// quote or another backslash, and a backslash outside quotes escapes the next
// character. That is enough for `exec api pytest -k "not slow"` and for a
// directory with a space in it.
//
// It is deliberately NOT a shell. There is no expansion of any kind: no
// globbing, no $VAR, no ~, no backticks, no pipes, no redirection, no `&&`. The
// command tree takes an argv vector and mabo-ctl execs vectors rather than shell
// strings — that property is why `cmd:` in mabo-ctl.yaml is a list — and a
// tokenizer that quietly grew a dialect would put a shell back underneath the
// one place the design took it out. A user who wants a shell has `shell <svc>`,
// which gives them a real one with the service's environment.
//
// An unterminated quote is an error naming which quote, because the alternative
// — silently treating the rest of the line as one argument — turns a typo into
// a command that runs with the wrong arguments.
func tokenize(line string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		started bool
	)
	emit := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			emit()
		case c == '\'':
			started = true
			j := i + 1
			for j < len(runes) && runes[j] != '\'' {
				cur.WriteRune(runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, fmt.Errorf("unbalanced ' in %q: close the quote, or escape it as \\'", line)
			}
			i = j
		case c == '"':
			started = true
			j := i + 1
			for j < len(runes) && runes[j] != '"' {
				if runes[j] == '\\' && j+1 < len(runes) && (runes[j+1] == '"' || runes[j+1] == '\\') {
					j++
				}
				cur.WriteRune(runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, fmt.Errorf("unbalanced \" in %q: close the quote, or escape it as \\\"", line)
			}
			i = j
		case c == '\\' && i+1 < len(runes):
			started = true
			i++
			cur.WriteRune(runes[i])
		default:
			started = true
			cur.WriteRune(c)
		}
	}
	emit()
	return args, nil
}
