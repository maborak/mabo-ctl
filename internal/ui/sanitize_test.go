package ui

import (
	"strings"
	"testing"
)

// TestSanitizeKeepsColour pins the deliberate survivor: SGR colour is the
// legitimate payload of a dev server's output.
func TestSanitizeKeepsColour(t *testing.T) {
	in := "\x1b[31mERROR\x1b[0m things broke"
	if got := Sanitize(in); got != in {
		t.Errorf("Sanitize stripped colour: %q", got)
	}
}

// TestSanitizeStripsTheHostileChannels covers the audit M-4 payloads byte for
// byte: an OSC-8 hyperlink, an OSC-52 clipboard write, a DCS channel, an
// unterminated OSC, and a bare C1 introducer.
func TestSanitizeStripsTheHostileChannels(t *testing.T) {
	cases := map[string]string{
		"OSC-8 hyperlink":  "\x1b]8;;https://evil.example\x1b\\click\x1b]8;;\x1b\\",
		"OSC-8 BEL form":   "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07",
		"OSC-52 clipboard": "\x1b]52;c;" + "SEVEN_BYTES" + "\x07",
		"OSC-0 title":      "\x1b]0;pwned-terminal\x07rest",
		"DCS":              "\x1bP+q544e\x1b\\after",
		"APC":              "\x1b_invisible-apc\x1b\\after",
		"unterminated OSC": "\x1b]52;c;leak-to-eol",
		"bare C1":          "\x9b31mnot-csi",
		"charset escape":   "\x1b(Bjunk-after-charset",
	}
	for name, in := range cases {
		if got := Sanitize(in); strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x9b) {
			t.Errorf("%s: Sanitize left an escape behind: %q", name, got)
		}
	}
	if got := Sanitize("\x1b]0;pwned-terminal\x07rest"); got != "rest" {
		t.Errorf("OSC-0: got %q, want the payload after the sequence", got)
	}
	if got := Sanitize("\x1bP+q544e\x1b\\after"); got != "after" {
		t.Errorf("DCS: got %q, want %q", got, "after")
	}
}

// TestSanitizeKeepsOrdinaryText is the fast-path guard: a line with no escape
// bytes at all must come back byte-identical, allocation-free decisions aside.
func TestSanitizeKeepsOrdinaryText(t *testing.T) {
	for _, in := range []string{
		"listening on 127.0.0.1:7999",
		"\rprogress: 42%",
		"col: 1\tcol: 2",
		"",
	} {
		if got := Sanitize(in); got != in {
			t.Errorf("Sanitize(%q) = %q, want unchanged", in, got)
		}
	}
}

// TestSanitizeKeepsMultibyteText is the regression for docs/LANDMINES.md §40:
// the C1 strip ran on bytes, and 0x80–0x9F are also UTF-8 continuation
// bytes, so every emoji and many symbols lost bytes and rendered as U+FFFD.
func TestSanitizeKeepsMultibyteText(t *testing.T) {
	for _, in := range []string{
		"18:55:09 \U0001F535 INFO  httpx | HTTP Request: GET /collections \"HTTP/1.1 200 OK\"",
		"price 12 € · señal · 日本語 · — · …",
		"\x1b[32m✅ ready\x1b[0m",            // colour and an emoji together (slow path)
		"\x1b[1m\U0001F680 deployed\x1b[0m", // 🚀: F0 9F 9A 80
	} {
		if got := Sanitize(in); got != in {
			t.Errorf("Sanitize(%q) = %q, want unchanged", in, got)
		}
	}
}

// TestSanitizeStripsEncodedC1: U+0080–U+009F encoded as UTF-8 (C2 80–C2 9F)
// are the C1 controls a UTF-8 terminal acts on; they go, whole, and the text
// around them stays.
func TestSanitizeStripsEncodedC1(t *testing.T) {
	for in, want := range map[string]string{
		"\u009b31mnot-csi":      "31mnot-csi",
		"a\u009d0;title\u009cb": "a0;titleb",
		"\x9b31mraw":            "31mraw",
		"é\x9bé":                "éé",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
