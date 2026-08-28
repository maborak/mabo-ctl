package ui

import "strings"

// Sanitize makes one line of supervised-child output safe to render on a
// terminal. Everything mabo-ctl composes is redacted at the source, but a
// child's own stdout is the child's — and a compromised dependency can write
// terminal control sequences that rewrite the operator's screen, hide log
// lines, or attempt an OSC-52 clipboard write the moment `logs` runs.
//
// What is stripped:
//
//   - OSC (`ESC ]` … BEL or ST) — hyperlinks, window titles, clipboard writes.
//   - DCS / APC / PM / SOS (`ESC P`, `ESC _`, `ESC ^`, `ESC X`) — payload
//     channels with no legitimate dev-server use. An unterminated one is
//     swallowed to end of line rather than leaking its tail.
//   - Bare C1 controls (0x80–0x9F) — single-byte introducers some terminals
//     treat as OSC/CSI, a parsing hazard with no printable use.
//   - Stray ESC bytes and non-CSI escape introducers (`ESC ( B` charsets and
//     friends), so no half-sequence survives as junk.
//
// What deliberately survives: CSI (`ESC [` … final byte) — colour and style
// are the legitimate payload of a dev server's output, and logs that lose
// their colour lose their usefulness. Newlines, tabs and carriage returns
// pass through for the same reason.
//
// Sanitize operates on RENDERED terminal streams only. The bytes on disk in
// `.dev/logs/` stay verbatim: they are evidence, and a pager can show them.
func Sanitize(s string) string {
	if !strings.ContainsRune(s, 0x1b) && !hasC1(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == 0x1b && i+1 >= len(s):
			// Lone ESC at end of line: nothing to introduce, keep nothing.
			i++
		case c == 0x1b && s[i+1] == '[':
			// CSI: copy through to the final byte (0x40–0x7E). An
			// unterminated sequence at end of line is dropped — a partial
			// escape is junk by definition.
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j < len(s) {
				j++
				b.WriteString(s[i:j])
			}
			i = j
		case c == 0x1b && containsByte("]P_^X", s[i+1]):
			// OSC/DCS/APC/PM/SOS: strip through BEL or ST (ESC \).
			i = swallowString(i, s)
		case c == 0x1b && containsByte("()*+-./#", s[i+1]):
			// nF/Fp escape with an intermediate byte: ESC ( B and friends
			// are three bytes.
			i += 3
		case c == 0x1b:
			// Any other two-byte escape.
			i += 2
		case c >= 0x80 && c < 0xa0:
			// Bare C1 control.
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// swallowString returns the index just past the string-terminated sequence
// starting at i (OSC and friends): through the first BEL, or through ESC \,
// or end of input when unterminated.
func swallowString(i int, s string) int {
	for j := i + 2; j < len(s); j++ {
		if s[j] == 0x07 {
			return j + 1
		}
		if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
			return j + 2
		}
	}
	return len(s)
}

// hasC1 reports whether s contains any C1 control byte.
func hasC1(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 && s[i] < 0xa0 {
			return true
		}
	}
	return false
}

// ContainsByte reports whether s contains the byte c. It is ContainsRune for
// ASCII bytes without the rune conversion.
func containsByte(s string, c byte) bool {
	return strings.IndexByte(s, c) >= 0
}
