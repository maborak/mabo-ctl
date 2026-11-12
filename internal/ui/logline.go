package ui

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/maborak/mabo-ctl/internal/redact"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// logLevelKeys and logLevelMap mirror LEVEL_KEYS and LEVEL_MAP in
// internal/web/console.html, so a line is the same level in the web console
// and the TUI. A test pins the two copies together.
var logLevelKeys = []string{"log.level", "level", "severity", "loglevel", "lvl"}

var logLevelMap = map[string]string{
	"error": "error", "err": "error", "fatal": "error", "critical": "error",
	"crit": "error", "emerg": "error", "alert": "error", "panic": "error",
	"warn": "warn", "warning": "warn",
	"debug": "debug", "trace": "debug", "verbose": "debug",
	"info": "info", "informational": "info", "notice": "info",
	"success": "info", "ok": "info", "succeeded": "info",
}

// LogLevels is the closed set LogLevel returns, most severe first.
var LogLevels = []string{"error", "warn", "info", "debug"}

// LogLevel classifies one log line as "error", "warn", "info" or "debug", or
// returns "" when it carries no recognised level. Only a JSON object can carry
// one: the first string-valued key of logLevelKeys wins, and a value outside
// logLevelMap leaves the line unclassified rather than guessing from a second
// key the service did not choose.
func LogLevel(text string) string {
	s := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(s, "{") {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return ""
	}
	for _, k := range logLevelKeys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		return logLevelMap[strings.ToLower(v)]
	}
	return ""
}

// IsJSONLine reports whether text is one complete JSON object or array, the
// lines PrettyJSON applies to.
func IsJSONLine(text string) bool {
	s := strings.TrimLeft(text, " \t\r\n")
	return (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) && json.Valid([]byte(s))
}

// PrettyJSON re-indents one JSON object or array by changing whitespace only,
// two spaces per level. It never decodes and re-encodes: that would round
// integers past 2^53 in the browser's copy of this function, collapse
// duplicate keys and reorder keys, showing values the service never wrote. It
// reports false, and the caller shows the raw text, when text is not exactly
// one balanced object or array — a line cut by a display limit, for one.
//
// It is the Go twin of prettyJSON in internal/web/console.html.
func PrettyJSON(text string) (string, bool) {
	var b strings.Builder
	b.Grow(len(text) + len(text)/4)
	var stack []byte
	inStr, esc, done := false, false, false
	pad := func(n int) {
		b.WriteByte('\n')
		for range n {
			b.WriteString("  ")
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		}
		// Only one top-level object or array, and nothing outside it.
		if done || (len(stack) == 0 && c != '{' && c != '[') {
			return "", false
		}
		switch c {
		case '"':
			inStr = true
			b.WriteByte(c)
		case '{', '[':
			closer := byte('}')
			if c == '[' {
				closer = ']'
			}
			j := i + 1
			for j < len(text) && strings.IndexByte(" \t\n\r", text[j]) >= 0 {
				j++
			}
			if j < len(text) && text[j] == closer {
				b.WriteByte(c)
				b.WriteByte(closer)
				i = j
				done = len(stack) == 0
				continue
			}
			stack = append(stack, closer)
			b.WriteByte(c)
			pad(len(stack))
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return "", false
			}
			stack = stack[:len(stack)-1]
			pad(len(stack))
			b.WriteByte(c)
			done = len(stack) == 0
		case ',':
			b.WriteByte(',')
			pad(len(stack))
		case ':':
			b.WriteString(": ")
		default:
			b.WriteByte(c)
		}
	}
	if inStr || len(stack) > 0 {
		return "", false
	}
	return b.String(), true
}

// RedactStatuses replaces each status's health URL with its redacted form, in
// place, and the same URL wherever Detail quotes it: a probe failure quotes the
// URL it dialled verbatim, so redacting Health alone would hand the credential
// out through the field next to it. Every reader that is not the operator's
// own one-shot CLI — the web console, the full-screen console, the MCP server —
// goes through this one function.
func RedactStatuses(sts []supervisor.Status) {
	for i := range sts {
		raw := sts[i].Health
		safe := redact.URL(raw)
		sts[i].Health = safe
		if raw != "" && raw != safe {
			sts[i].Detail = strings.ReplaceAll(sts[i].Detail, raw, safe)
		}
	}
}

// ShortDuration renders d for a status cell: "850ms", "1.2s", "4m05s",
// "2h03m". Zero renders as "".
func ShortDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return formatDuration(d)
}
