package ui

import "strings"

// ShellLine renders argv as one line a human can copy into a shell and get the
// same process. Arguments are quoted only when they need it, because the point
// of showing the command is that it is readable.
//
// It lives here, rather than beside either of its callers, because the web
// console and `mabo-ctl config` both show the resolved command and must show it
// the same way: two quoters would eventually disagree about one argument, and
// the one a developer pasted would be the wrong one.
//
// It is not a substitute for the argv itself. mabo-ctl execs a vector and never a
// shell string, so this is display only.
func ShellLine(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote single-quotes an argument when it contains anything a shell would
// interpret. A single quote inside is closed, escaped and reopened, which is
// the only form that is safe in every POSIX shell.
func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, needsQuoting) < 0 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// needsQuoting reports whether r must be quoted to survive a shell.
func needsQuoting(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("@%+=:,./-_", r)
}
