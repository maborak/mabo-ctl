package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// env builds a lookup function over a fixed environment for the colour policy
// tests, so they never depend on the environment the suite happens to run in.
func env(pairs ...string) func(string) (string, bool) {
	m := make(map[string]string, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// lines splits a rendered block into its lines.
func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// runeIndex returns the rune offset of sub in s, or -1.
func runeIndex(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return utf8.RuneCountInString(s[:i])
}

func status(name string, phase supervisor.Phase) supervisor.Status {
	return supervisor.Status{Name: name, Phase: phase, LogPath: "/repo/.dev/logs/" + name + ".log"}
}

// --- colour detection -------------------------------------------------------

func TestColorPolicy(t *testing.T) {
	cases := []struct {
		name     string
		terminal bool
		lookup   func(string) (string, bool)
		want     bool
	}{
		{"tty with a real term", true, env("TERM", "xterm-256color"), true},
		{"NO_COLOR set to 1 disables", true, env("TERM", "xterm-256color", "NO_COLOR", "1"), false},
		{"NO_COLOR empty still disables", true, env("TERM", "xterm-256color", "NO_COLOR", ""), false},
		{"NO_COLOR=0 still disables", true, env("TERM", "xterm-256color", "NO_COLOR", "0"), false},
		{"TERM=dumb disables", true, env("TERM", "dumb"), false},
		{"TERM unset disables", true, env(), false},
		{"TERM empty disables", true, env("TERM", ""), false},
		{"not a terminal disables", false, env("TERM", "xterm-256color"), false},
		{"not a terminal and NO_COLOR", false, env("TERM", "xterm-256color", "NO_COLOR", "yes"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := colorEnabled(tc.terminal, tc.lookup); got != tc.want {
				t.Fatalf("colorEnabled(terminal=%v) = %v, want %v", tc.terminal, got, tc.want)
			}
		})
	}
}

func TestNewDisablesColorOnAPipe(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLUMNS", "120")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})

	if got := New(w); got.Color {
		t.Fatal("New(pipe).Color = true, want false: escape sequences must never go into a pipe")
	}
	if got := New(w); got.Width != 120 {
		t.Fatalf("New(pipe).Width = %d, want 120 from COLUMNS", got.Width)
	}
}

func TestNewDisablesColorUnderNoColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")

	// A pipe is not a terminal either, but NO_COLOR must be sufficient on its
	// own: the policy is checked directly with terminal=true as well.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	if New(w).Color {
		t.Fatal("New with NO_COLOR set enabled colour")
	}
	if colorEnabled(true, os.LookupEnv) {
		t.Fatal("colorEnabled(terminal=true) with NO_COLOR set enabled colour")
	}
}

func TestNewNilFileIsPlain(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	r := New(nil)
	if r.Color {
		t.Fatal("New(nil).Color = true, want false")
	}
}

func TestEnvWidth(t *testing.T) {
	cases := []struct {
		in   func(string) (string, bool)
		want int
	}{
		{env(), 0},
		{env("COLUMNS", "100"), 100},
		{env("COLUMNS", " 80 "), 80},
		{env("COLUMNS", "0"), 0},
		{env("COLUMNS", "-5"), 0},
		{env("COLUMNS", "wide"), 0},
	}
	for _, tc := range cases {
		if got := envWidth(tc.in); got != tc.want {
			t.Errorf("envWidth = %d, want %d", got, tc.want)
		}
	}
}

func TestIsTerminalOnAPipeAndAFile(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})
	if isTerminal(pw) {
		t.Error("isTerminal(pipe) = true")
	}

	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if isTerminal(f) {
		t.Error("isTerminal(regular file) = true")
	}
	if isTerminal(nil) {
		t.Error("isTerminal(nil) = true")
	}
}

// --- status block -----------------------------------------------------------

func TestStatusBlockEmpty(t *testing.T) {
	r := &Renderer{}
	if got := r.StatusBlock(nil); got != "" {
		t.Fatalf("StatusBlock(nil) = %q, want empty", got)
	}
}

func TestStatusBlockPadsNamesToTheLongest(t *testing.T) {
	r := &Renderer{}
	block := r.StatusBlock([]supervisor.Status{
		{Name: "a", Phase: supervisor.PhaseReady, PID: 1, Port: 7100, Elapsed: time.Second},
		{Name: "verylongservicename", Phase: supervisor.PhaseSlow, PID: 222222, Port: 7101},
		{Name: "mid", Phase: supervisor.PhaseStopped},
	})

	got := lines(block)
	if len(got) != 4 {
		t.Fatalf("got %d lines, want 4 (header + 3 services):\n%s", len(got), block)
	}

	// The label column is the longest name, so every status glyph starts at the
	// same offset — that alignment is the whole point of the block.
	const wantStart = len("verylongservicename") + len(colGap)
	if at := runeIndex(got[0], hdrStatus); at != wantStart {
		t.Errorf("header STATUS starts at rune %d, want %d\n%s", at, wantStart, block)
	}
	for i, phase := range []supervisor.Phase{supervisor.PhaseReady, supervisor.PhaseSlow, supervisor.PhaseStopped} {
		glyph, _ := PhaseLabel(phase)
		if at := runeIndex(got[i+1], glyph); at != wantStart {
			t.Errorf("line %d: glyph %q starts at rune %d, want %d\n%s", i+1, glyph, at, wantStart, block)
		}
	}

	// The port column is aligned too: both ports sit at the same offset.
	first := runeIndex(got[1], "7100")
	second := runeIndex(got[2], "7101")
	if first != second || first < 0 {
		t.Errorf("port column not aligned: %d vs %d\n%s", first, second, block)
	}
}

func TestStatusBlockPadsShortNamesToTheHeader(t *testing.T) {
	r := &Renderer{}
	block := r.StatusBlock([]supervisor.Status{
		{Name: "a", Phase: supervisor.PhaseReady},
		{Name: "bb", Phase: supervisor.PhaseReady},
	})
	got := lines(block)
	glyph, _ := PhaseLabel(supervisor.PhaseReady)
	want := len(hdrService) + len(colGap)
	for i := 1; i < len(got); i++ {
		if at := runeIndex(got[i], glyph); at != want {
			t.Fatalf("line %d: glyph at rune %d, want %d\n%s", i, at, want, block)
		}
	}
}

func TestEveryPhaseIsDistinguishableWithoutColour(t *testing.T) {
	// Every phase the supervisor can report, not a list maintained here: a
	// phase added on the server without a glyph in this package must fail HERE
	// rather than reach a reader as "?".
	phases := supervisor.Phases()
	if len(phases) < 7 {
		t.Fatalf("supervisor.Phases() returned %d phases; the enum lost one", len(phases))
	}

	sts := make([]supervisor.Status, 0, len(phases))
	for _, p := range phases {
		sts = append(sts, status(string(p)+"-svc", p))
	}

	r := &Renderer{Color: false}
	block := r.StatusBlock(sts)
	if strings.Contains(block, "\x1b") {
		t.Fatalf("colour is off but the block contains an escape sequence:\n%q", block)
	}

	seenGlyph := map[string]supervisor.Phase{}
	seenWord := map[string]supervisor.Phase{}
	body := lines(block)[1:]
	for i, p := range phases {
		glyph, word := PhaseLabel(p)
		if glyph == "" || word == "" {
			t.Fatalf("phase %q has an empty glyph or word", p)
		}
		if other, dup := seenGlyph[glyph]; dup {
			t.Errorf("phase %q reuses the glyph %q of %q", p, glyph, other)
		}
		if other, dup := seenWord[word]; dup {
			t.Errorf("phase %q reuses the word %q of %q", p, word, other)
		}
		seenGlyph[glyph] = p
		seenWord[word] = p

		if !strings.Contains(body[i], glyph+" "+word) {
			t.Errorf("line %q does not carry %q + %q", body[i], glyph, word)
		}
	}

	if len(seenGlyph) != len(phases) || len(seenWord) != len(phases) {
		t.Fatalf("got %d glyphs and %d words for %d phases", len(seenGlyph), len(seenWord), len(phases))
	}
}

func TestPhaseLabelForAnUnknownPhase(t *testing.T) {
	glyph, word := PhaseLabel(supervisor.Phase("wat"))
	if glyph != "?" || word != "wat" {
		t.Fatalf("PhaseLabel(wat) = %q, %q; want \"?\", \"wat\"", glyph, word)
	}
	glyph, word = PhaseLabel("")
	if glyph != "?" || word != "unknown" {
		t.Fatalf("PhaseLabel(\"\") = %q, %q; want \"?\", \"unknown\"", glyph, word)
	}
}

func TestStatusBlockColouredColumnsStillAlign(t *testing.T) {
	r := &Renderer{Color: true}
	r.SetServiceColors(map[string]string{"web": "green", "api": "blue"})
	block := r.StatusBlock([]supervisor.Status{
		{Name: "web", Phase: supervisor.PhaseReady, Port: 7100, PID: 10},
		{Name: "api", Phase: supervisor.PhaseFailed, Port: 7102, PID: 11},
	})
	if !strings.Contains(block, "\x1b[") {
		t.Fatal("colour is on but no escape sequence was emitted")
	}

	plain := lines(stripANSI(block))
	glyphReady, _ := PhaseLabel(supervisor.PhaseReady)
	glyphFailed, _ := PhaseLabel(supervisor.PhaseFailed)
	if a, b := runeIndex(plain[1], glyphReady), runeIndex(plain[2], glyphFailed); a != b || a < 0 {
		t.Fatalf("coloured rows misaligned: %d vs %d\n%s", a, b, stripANSI(block))
	}

	// Padding must sit outside the escape sequence, or a terminal that ignores
	// SGR would still see the right number of spaces but a styled one would not.
	if strings.Contains(block, " \x1b[0m") {
		t.Error("padding was painted along with the cell text")
	}
}

func TestStatusBlockRendersPortPIDAndUptime(t *testing.T) {
	r := &Renderer{}
	block := r.StatusBlock([]supervisor.Status{
		{Name: "web", Phase: supervisor.PhaseReady, Port: 7100, PID: 4242, HTTP: 200, Elapsed: 1200 * time.Millisecond},
		{Name: "worker", Phase: supervisor.PhaseStopped},
	})
	got := lines(block)
	for _, want := range []string{"7100", "4242", "1.2s", "HTTP 200"} {
		if !strings.Contains(got[1], want) {
			t.Errorf("row %q does not contain %q", got[1], want)
		}
	}
	// A stopped service has no port, pid or uptime: they must read as absent,
	// never as a zero that looks like a real value.
	if strings.Contains(got[2], "0") {
		t.Errorf("stopped row %q printed a zero instead of %q", got[2], missing)
	}
	if n := strings.Count(got[2], missing); n != 3 {
		t.Errorf("stopped row %q has %d %q cells, want 3", got[2], n, missing)
	}
}

func TestStatusBlockSlowShowsWhatItIsWaitingFor(t *testing.T) {
	r := &Renderer{}
	block := r.StatusBlock([]supervisor.Status{{
		Name:   "web",
		Phase:  supervisor.PhaseSlow,
		Port:   7100,
		PID:    5,
		Health: "http://localhost:7100/robots.txt",
	}})
	if !strings.Contains(block, "waiting for http://localhost:7100/robots.txt") {
		t.Fatalf("slow row does not name the URL it is waiting on:\n%s", block)
	}
}

func TestStatusBlockKeepsAMultiLineDetailOutOfTheColumns(t *testing.T) {
	r := &Renderer{}
	block := r.StatusBlock([]supervisor.Status{
		{
			Name:   "backend",
			Phase:  supervisor.PhaseFailed,
			Detail: "process died\nTraceback (most recent call last):\n  ImportError: no module named api_main",
		},
		{Name: "web", Phase: supervisor.PhaseReady, Port: 7100, PID: 3},
	})
	got := lines(block)
	if len(got) != 5 {
		t.Fatalf("got %d lines, want 5 (header + failed row + 2 continuations + ready row):\n%s", len(got), block)
	}
	if !strings.Contains(got[1], "process died") {
		t.Errorf("first detail line is not in the row: %q", got[1])
	}
	for _, i := range []int{2, 3} {
		if !strings.HasPrefix(got[i], detailIndent) {
			t.Errorf("continuation line %d is not indented: %q", i, got[i])
		}
	}
	// The row after a multi-line detail must still be aligned with the header.
	glyph, _ := PhaseLabel(supervisor.PhaseReady)
	if runeIndex(got[4], glyph) != runeIndex(got[0], hdrStatus) {
		t.Errorf("row after a multi-line detail lost its alignment:\n%s", block)
	}
}

func TestStatusBlockTruncatesTheDetailToWidth(t *testing.T) {
	const w = 100
	r := &Renderer{Width: w}
	block := r.StatusBlock([]supervisor.Status{{
		Name:   "web",
		Phase:  supervisor.PhaseReady,
		Port:   7100,
		PID:    12345,
		Detail: strings.Repeat("x", 400),
	}})
	row := lines(block)[1]
	if n := utf8.RuneCountInString(row); n > w {
		t.Fatalf("row is %d columns wide, want at most %d:\n%s", n, w, row)
	}
	if !strings.HasSuffix(row, "…") {
		t.Fatalf("truncated row does not end with an ellipsis: %q", row)
	}
	// A service name is never truncated, however narrow the terminal claims to be.
	narrow := (&Renderer{Width: 10}).StatusBlock([]supervisor.Status{{
		Name: "web", Phase: supervisor.PhaseReady, Detail: strings.Repeat("y", 80),
	}})
	if !strings.Contains(narrow, "web") {
		t.Fatalf("narrow block dropped the service name:\n%s", narrow)
	}
}

func TestStatusBlockNoTrailingWhitespace(t *testing.T) {
	r := &Renderer{Color: true}
	block := r.StatusBlock([]supervisor.Status{
		{Name: "web", Phase: supervisor.PhaseReady, Port: 7100, PID: 1},
		{Name: "worker", Phase: supervisor.PhaseStopped},
	})
	for i, l := range lines(block) {
		if strings.TrimRight(l, " ") != l {
			t.Errorf("line %d has trailing whitespace: %q", i, l)
		}
	}
	if strings.HasSuffix(block, "\n") {
		t.Error("StatusBlock ends with a newline")
	}
}

func TestStatusBlockPreservesOrder(t *testing.T) {
	r := &Renderer{}
	block := r.StatusBlock([]supervisor.Status{
		status("zeta", supervisor.PhaseReady),
		status("alpha", supervisor.PhaseReady),
	})
	got := lines(block)
	if !strings.HasPrefix(got[1], "zeta") || !strings.HasPrefix(got[2], "alpha") {
		t.Fatalf("StatusBlock reordered the services:\n%s", block)
	}
}

func TestStatusBlockColumnsDoNotMoveBetweenInvocations(t *testing.T) {
	r := &Renderer{}
	// The same services, once all ready and once with the widest phase word.
	allReady := r.StatusBlock([]supervisor.Status{
		{Name: "web", Phase: supervisor.PhaseReady, Port: 7100, PID: 1},
		{Name: "api", Phase: supervisor.PhaseReady, Port: 7102, PID: 2},
	})
	mixed := r.StatusBlock([]supervisor.Status{
		{Name: "web", Phase: supervisor.PhaseStopped, Port: 7100},
		{Name: "api", Phase: supervisor.PhaseRunning, Port: 7102, PID: 2},
	})
	if a, b := runeIndex(lines(allReady)[0], hdrPort), runeIndex(lines(mixed)[0], hdrPort); a != b {
		t.Fatalf("the PORT column moved between invocations: %d vs %d\n%s\n%s", a, b, allReady, mixed)
	}
	if a, b := runeIndex(lines(allReady)[1], "7100"), runeIndex(lines(mixed)[1], "7100"); a != b {
		t.Fatalf("a port moved between invocations: %d vs %d\n%s\n%s", a, b, allReady, mixed)
	}
}

// --- events -----------------------------------------------------------------

func TestEvent(t *testing.T) {
	r := &Renderer{}
	r.SetServiceColors(map[string]string{"web": "green", "backend": "blue"})

	got := r.Event(supervisor.Event{Service: "web", Phase: supervisor.PhaseReady, Msg: "listening on :7100"})
	glyph, word := PhaseLabel(supervisor.PhaseReady)
	if !strings.Contains(got, glyph+" "+word) || !strings.Contains(got, "listening on :7100") {
		t.Fatalf("Event = %q", got)
	}
	// Labels are padded to the longest known service so streamed events line up.
	if !strings.HasPrefix(got, "web"+strings.Repeat(" ", len("backend")-len("web"))) {
		t.Fatalf("Event label is not padded to the longest service name: %q", got)
	}

	// Two events with different phases put their message at the same offset.
	stopped := r.Event(supervisor.Event{Service: "web", Phase: supervisor.PhaseStopped, Msg: "MSG"})
	if a, b := runeIndex(got, "listening"), runeIndex(stopped, "MSG"); a != b {
		t.Errorf("event messages are not aligned: %d vs %d\n%q\n%q", a, b, got, stopped)
	}

	withErr := r.Event(supervisor.Event{Service: "backend", Phase: supervisor.PhaseFailed, Msg: "start failed", Err: errors.New("boom")})
	if !strings.Contains(withErr, "boom") {
		t.Fatalf("Event dropped the error: %q", withErr)
	}

	global := r.Event(supervisor.Event{Msg: "reset complete"})
	if global != "reset complete" {
		t.Fatalf("global Event = %q, want %q", global, "reset complete")
	}
	if got := r.Event(supervisor.Event{}); got != "" {
		t.Fatalf("empty Event = %q, want empty", got)
	}
}

// --- port origins -----------------------------------------------------------

func TestPortOriginsPrintsNothingWithoutAnOverride(t *testing.T) {
	r := &Renderer{}
	origins := []service.Origin{
		{Service: "web", Port: 7100, Source: service.FromDefault, Declared: 7100},
		{Service: "backend", Port: 7999, Source: service.FromFlag, Declared: 7102},
		{Service: "worker", Port: 0, Source: service.FromDefault},
		{Service: "browser", Port: 7103, Source: service.FromRunEnv, Declared: 7103},
	}
	if got := r.PortOrigins(origins); got != "" {
		t.Fatalf("PortOrigins printed %q; only run.env overrides are worth a line", got)
	}
	if got := r.PortOrigins(nil); got != "" {
		t.Fatalf("PortOrigins(nil) = %q, want empty", got)
	}
}

func TestPortOriginsNamesBothPortsAndHowToClearThem(t *testing.T) {
	r := &Renderer{}
	got := r.PortOrigins([]service.Origin{
		{Service: "web", Port: 7100, Source: service.FromDefault, Declared: 7100},
		{Service: "backend", Port: 7002, Source: service.FromRunEnv, Declared: 7102, Override: true},
	})
	if got == "" {
		t.Fatal("PortOrigins printed nothing for an override")
	}
	for _, want := range []string{"backend", "7002", "7102", string(service.FromRunEnv), "mabo-ctl reset", ".dev/run.env"} {
		if !strings.Contains(got, want) {
			t.Errorf("PortOrigins output does not mention %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "web") {
		t.Errorf("PortOrigins mentioned a service that is not overridden:\n%s", got)
	}
	if n := len(lines(got)); n != 2 {
		t.Errorf("got %d lines, want 2 (one override + the hint):\n%s", n, got)
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("colour is off but PortOrigins emitted an escape sequence: %q", got)
	}
}

// --- errors -----------------------------------------------------------------

func TestErrorRendersEveryValidationProblemAsABullet(t *testing.T) {
	r := &Renderer{}
	ve := &config.ValidationError{
		Path: "/repo/mabo-ctl.yaml",
		Problems: []string{
			`service "we/b": name must match ^[a-zA-Z0-9][a-zA-Z0-9_-]*$`,
			`service "backend": dir "missing" does not exist`,
			`service "worker": cmd is empty`,
		},
	}
	got := r.Error(fmt.Errorf("loading config: %w", ve))

	body := lines(got)
	if len(body) != 4 {
		t.Fatalf("got %d lines, want 1 headline + 3 bullets:\n%s", len(body), got)
	}
	if !strings.Contains(body[0], "/repo/mabo-ctl.yaml") || !strings.Contains(body[0], "3 problems") {
		t.Errorf("headline does not name the file and the count: %q", body[0])
	}
	for i, p := range ve.Problems {
		if !strings.Contains(body[i+1], p) {
			t.Errorf("bullet %d = %q, want it to contain %q", i, body[i+1], p)
		}
		if !strings.Contains(body[i+1], "•") {
			t.Errorf("bullet %d is not bulleted: %q", i, body[i+1])
		}
	}
}

func TestErrorSingleValidationProblemReadsSingular(t *testing.T) {
	r := &Renderer{}
	got := r.Error(&config.ValidationError{Path: "/repo/mabo-ctl.yaml", Problems: []string{"only one"}})
	if !strings.Contains(got, "(1 problem)") || strings.Contains(got, "problems") {
		t.Fatalf("Error = %q, want a singular headline", got)
	}
}

func TestErrorOnAPlainErrorIsOneLine(t *testing.T) {
	r := &Renderer{}
	got := r.Error(errors.New("port 7100 is held by pid 812"))
	if n := len(lines(got)); n != 1 {
		t.Fatalf("got %d lines, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "port 7100 is held by pid 812") {
		t.Fatalf("Error dropped the message: %q", got)
	}
}

func TestErrorOnAJoinedErrorBulletsEveryBranch(t *testing.T) {
	r := &Renderer{}
	got := r.Error(errors.Join(errors.New("first"), errors.New("second")))
	if n := len(lines(got)); n != 3 {
		t.Fatalf("got %d lines, want a headline and 2 bullets:\n%s", n, got)
	}
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(got, want) {
			t.Errorf("joined error dropped %q:\n%s", want, got)
		}
	}
}

func TestErrorNil(t *testing.T) {
	if got := (&Renderer{}).Error(nil); got != "" {
		t.Fatalf("Error(nil) = %q, want empty", got)
	}
}

// --- the machine contract ---------------------------------------------------

// goldenStatusJSON is the exact wire form of goldenStatuses. It is written out
// in full, on purpose: `mabo-ctl status --json` is the documented integration
// point, so a renamed or reordered field must fail here rather than in somebody
// else's script.
const goldenStatusJSON = `[
  {
    "service": "website",
    "phase": "ready",
    "pid": 41231,
    "port": 7100,
    "health": "http://localhost:7100/robots.txt?probe=1&fast=true",
    "http_status": 200,
    "detail": "",
    "log_path": "/repo/.dev/logs/website.log",
    "elapsed_ms": 1200,
    "started_at": "2024-03-01T09:12:00Z",
    "uptime_ms": 3600000,
    "exit_code": 0,
    "exit_signal": "",
    "exited_at": ""
  },
  {
    "service": "worker",
    "phase": "failed",
    "pid": 0,
    "port": 0,
    "health": "",
    "http_status": 0,
    "detail": "process died\nlog is empty",
    "log_path": "/repo/.dev/logs/worker.log",
    "elapsed_ms": 0,
    "started_at": "",
    "uptime_ms": 0,
    "exit_code": 0,
    "exit_signal": "",
    "exited_at": ""
  },
  {
    "service": "backend",
    "phase": "exited",
    "pid": 0,
    "port": 7102,
    "health": "http://localhost:7102/health",
    "http_status": 0,
    "detail": "killed by SIGKILL, 4m ago",
    "log_path": "/repo/.dev/logs/backend.log",
    "elapsed_ms": 0,
    "started_at": "2024-03-01T09:12:00Z",
    "uptime_ms": 0,
    "exit_code": -1,
    "exit_signal": "SIGKILL",
    "exited_at": "2024-03-01T10:12:00Z"
  }
]`

// goldenSpawn and goldenDeath are fixed instants, not time.Now(): the golden
// above is compared byte for byte, so a timestamp that moved would fail this
// test every run for a reason that has nothing to do with the contract.
var (
	goldenSpawn = time.Date(2024, 3, 1, 9, 12, 0, 0, time.UTC)
	goldenDeath = goldenSpawn.Add(time.Hour)
)

func goldenStatuses() []supervisor.Status {
	return []supervisor.Status{
		{
			Name:      "website",
			Phase:     supervisor.PhaseReady,
			PID:       41231,
			Port:      7100,
			Health:    "http://localhost:7100/robots.txt?probe=1&fast=true",
			HTTP:      200,
			LogPath:   "/repo/.dev/logs/website.log",
			Elapsed:   1200 * time.Millisecond,
			StartedAt: goldenSpawn,
			Uptime:    time.Hour,
		},
		{
			Name:    "worker",
			Phase:   supervisor.PhaseFailed,
			Detail:  "process died\nlog is empty",
			LogPath: "/repo/.dev/logs/worker.log",
		},
		{
			// The phase that did not exist before this contract carried it: a
			// service mabo-ctl started, that came up, and that is gone without
			// mabo-ctl stopping it.
			Name:       "backend",
			Phase:      supervisor.PhaseExited,
			Port:       7102,
			Health:     "http://localhost:7102/health",
			Detail:     "killed by SIGKILL, 4m ago",
			LogPath:    "/repo/.dev/logs/backend.log",
			StartedAt:  goldenSpawn,
			ExitCode:   -1,
			ExitSignal: "SIGKILL",
			ExitedAt:   goldenDeath,
		},
	}
}

func TestStatusJSONGolden(t *testing.T) {
	got, err := StatusJSON(goldenStatuses())
	if err != nil {
		t.Fatalf("StatusJSON: %v", err)
	}
	if string(got) != goldenStatusJSON {
		t.Fatalf("StatusJSON is a STABLE CONTRACT and it changed.\n got:\n%s\nwant:\n%s", got, goldenStatusJSON)
	}
}

func TestStatusJSONFieldNamesAndOrder(t *testing.T) {
	// Field names, spelled out so a rename cannot pass by editing one constant.
	// The five after elapsed_ms were APPENDED, which is the only
	// backwards-compatible way to change this contract: an existing consumer
	// reads the first nine exactly as it did before.
	want := []string{
		"service", "phase", "pid", "port", "health", "http_status", "detail",
		"log_path", "elapsed_ms",
		"started_at", "uptime_ms", "exit_code", "exit_signal", "exited_at",
	}

	raw, err := StatusJSON(goldenStatuses())
	if err != nil {
		t.Fatalf("StatusJSON: %v", err)
	}

	var records []map[string]any
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatalf("StatusJSON is not valid JSON: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	for i, rec := range records {
		if len(rec) != len(want) {
			t.Fatalf("record %d has %d fields, want %d: %v", i, len(rec), len(want), rec)
		}
		for _, k := range want {
			if _, ok := rec[k]; !ok {
				t.Errorf("record %d is missing the field %q", i, k)
			}
		}
	}

	// Order is part of the contract too, so a diff of two runs stays readable.
	text := string(raw)
	prev := -1
	for _, k := range want {
		at := strings.Index(text, `"`+k+`"`)
		if at < 0 {
			t.Fatalf("field %q is absent from the encoded output", k)
		}
		if at <= prev {
			t.Fatalf("field %q is out of order in the encoded output", k)
		}
		prev = at
	}
}

// TestStatusJSONCarriesTheCrashFields checks the five fields the crash-visibility
// work appended, because "the golden matched" would also be true if they were
// all zero.
func TestStatusJSONCarriesTheCrashFields(t *testing.T) {
	raw, err := StatusJSON(goldenStatuses())
	if err != nil {
		t.Fatalf("StatusJSON: %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatalf("StatusJSON is not valid JSON: %v", err)
	}

	live, dead := records[0], records[2]

	if got := live["uptime_ms"]; got != float64(time.Hour.Milliseconds()) {
		t.Errorf("uptime_ms = %v, want %d", got, time.Hour.Milliseconds())
	}
	if got, err := time.Parse(time.RFC3339, live["started_at"].(string)); err != nil {
		t.Errorf("started_at does not parse as RFC 3339: %v", err)
	} else if !got.Equal(goldenSpawn) {
		t.Errorf("started_at = %s, want %s", got, goldenSpawn)
	}
	// A running service has no exit to report, and the field that says so is
	// exited_at: exit_code 0 is indistinguishable from a clean exit.
	if got := live["exited_at"]; got != "" {
		t.Errorf("a running service reports exited_at = %q, want empty", got)
	}

	if got := dead["phase"]; got != string(supervisor.PhaseExited) {
		t.Errorf("phase = %v, want %q", got, supervisor.PhaseExited)
	}
	if got := dead["exit_signal"]; got != "SIGKILL" {
		t.Errorf("exit_signal = %v, want SIGKILL", got)
	}
	if got := dead["exit_code"]; got != float64(-1) {
		t.Errorf("exit_code = %v, want -1 for a signalled process", got)
	}
	if got, err := time.Parse(time.RFC3339, dead["exited_at"].(string)); err != nil {
		t.Errorf("exited_at does not parse as RFC 3339: %v", err)
	} else if !got.Equal(goldenDeath) {
		t.Errorf("exited_at = %s, want %s", got, goldenDeath)
	}
	// Nothing is running, so there is no uptime to report — never a duration
	// counted from the death.
	if got := dead["uptime_ms"]; got != float64(0) {
		t.Errorf("an exited service reports uptime_ms = %v, want 0", got)
	}
}

func TestStatusJSONCarriesNoPresentation(t *testing.T) {
	r := &Renderer{Color: true}
	r.SetServiceColors(map[string]string{"website": "green"})
	_ = r.StatusBlock(goldenStatuses()) // colour state must not leak anywhere

	raw, err := StatusJSON(goldenStatuses())
	if err != nil {
		t.Fatalf("StatusJSON: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "\x1b") {
		t.Error("StatusJSON contains an escape sequence")
	}
	for _, glyph := range []string{"●", "◐", "○", "✕", "◆", "⚠", "⊘", "…"} {
		if strings.Contains(text, glyph) {
			t.Errorf("StatusJSON contains the presentation glyph %q", glyph)
		}
	}
	if strings.Contains(text, "  \"service\":  ") {
		t.Error("StatusJSON pads its values")
	}
}

func TestStatusJSONDoesNotEscapeURLCharacters(t *testing.T) {
	raw, err := StatusJSON(goldenStatuses())
	if err != nil {
		t.Fatalf("StatusJSON: %v", err)
	}
	if !strings.Contains(string(raw), "?probe=1&fast=true") {
		t.Fatalf("a health URL was HTML-escaped:\n%s", raw)
	}
}

func TestStatusJSONEmptyIsAnArrayNotNull(t *testing.T) {
	for _, in := range [][]supervisor.Status{nil, {}} {
		raw, err := StatusJSON(in)
		if err != nil {
			t.Fatalf("StatusJSON: %v", err)
		}
		if string(raw) != "[]" {
			t.Fatalf("StatusJSON(%v) = %q, want %q", in, raw, "[]")
		}
	}
}

func TestStatusJSONIsDeterministic(t *testing.T) {
	first, err := StatusJSON(goldenStatuses())
	if err != nil {
		t.Fatalf("StatusJSON: %v", err)
	}
	for range 5 {
		again, err := StatusJSON(goldenStatuses())
		if err != nil {
			t.Fatalf("StatusJSON: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("StatusJSON is not deterministic across calls")
		}
	}
}

// --- helpers ----------------------------------------------------------------

func TestServiceColoursAreDeclaredThenStable(t *testing.T) {
	r := &Renderer{Color: true}
	r.SetServiceColors(map[string]string{"web": "green", "api": ""})

	if got := r.serviceStyle("web"); got.params != "32" {
		t.Errorf("declared colour ignored: %q", got.params)
	}
	// An undeclared or blank colour still produces one, and the same one twice.
	blank, unknown := r.serviceStyle("api"), r.serviceStyle("nowhere")
	if blank.params == "" || unknown.params == "" {
		t.Fatal("a service without a declared colour got no colour at all")
	}
	if again := r.serviceStyle("nowhere"); again != unknown {
		t.Error("the fallback colour is not stable for the same name")
	}
}

func TestSetInstanceColors(t *testing.T) {
	r := &Renderer{Color: true}
	r.SetInstanceColors([]service.Instance{
		{Name: "backend", Color: "blue"},
		{Name: "web", Color: "green"},
	})
	if got := r.serviceStyle("backend"); got.params != "34" {
		t.Errorf("backend colour = %q, want 34", got.params)
	}
	if got := r.ServiceLabel("web"); got != "\x1b[32mweb\x1b[0m    " {
		t.Errorf("ServiceLabel(web) = %q, want it painted and padded to len(\"backend\")", got)
	}
}

func TestSetServiceColorsCopiesTheMap(t *testing.T) {
	r := &Renderer{Color: true}
	m := map[string]string{"web": "green"}
	r.SetServiceColors(m)
	m["web"] = "red"
	if got := r.serviceStyle("web"); got.params != "32" {
		t.Fatalf("mutating the caller's map changed the renderer: %q", got.params)
	}
}

func TestParseColor(t *testing.T) {
	cases := []struct {
		in     string
		params string
		ok     bool
	}{
		{"green", "32", true},
		{"GREEN", "32", true},
		{" cyan ", "36", true},
		{"gray", "90", true},
		{"grey", "90", true},
		{"bright-blue", "94", true},
		{"bright_blue", "94", true},
		{"bright-gray", "90", true},
		{"33", "38;5;33", true},
		{"255", "38;5;255", true},
		{"#ff8800", "38;2;255;136;0", true},
		{"", "", false},
		{"chartreuse", "", false},
		{"256", "", false},
		{"#fff", "", false},
		{"#gggggg", "", false},
	}
	for _, tc := range cases {
		got, ok := parseColor(tc.in)
		if ok != tc.ok || got.params != tc.params {
			t.Errorf("parseColor(%q) = (%q, %v), want (%q, %v)", tc.in, got.params, ok, tc.params, tc.ok)
		}
	}
}

func TestPaintIsInertWithoutColour(t *testing.T) {
	plain := &Renderer{Color: false}
	if got := plain.paint(style{"31"}, "text"); got != "text" {
		t.Fatalf("paint with colour off = %q", got)
	}
	coloured := &Renderer{Color: true}
	if got := coloured.paint(style{"31"}, "text"); got != "\x1b[31mtext\x1b[0m" {
		t.Fatalf("paint with colour on = %q", got)
	}
	if got := coloured.paint(style{}, "text"); got != "text" {
		t.Fatalf("paint with an empty style = %q", got)
	}
}

func TestWidthAndPadIgnoreEscapeSequences(t *testing.T) {
	painted := "\x1b[32mweb\x1b[0m"
	if got := width(painted); got != 3 {
		t.Fatalf("width(painted) = %d, want 3", got)
	}
	if got := pad(painted, 6); got != painted+"   " {
		t.Fatalf("pad(painted, 6) = %q", got)
	}
	if got := pad("toolong", 3); got != "toolong" {
		t.Fatalf("pad must never truncate, got %q", got)
	}
	if got := width("●"); got != 1 {
		t.Fatalf("width(glyph) = %d, want 1", got)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want string
	}{
		{"abcdef", 0, "abcdef"},
		{"abcdef", -1, "abcdef"},
		{"abcdef", 6, "abcdef"},
		{"abcdef", 3, "ab…"},
		{"abcdef", 1, "…"},
		{"héllo", 3, "hé…"},
	}
	for _, tc := range cases {
		if got := truncate(tc.in, tc.w); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, missing},
		{-time.Second, missing},
		{500 * time.Microsecond, "<1ms"},
		{820 * time.Millisecond, "820ms"},
		{1200 * time.Millisecond, "1.2s"},
		{59500 * time.Millisecond, "59.5s"},
		{200 * time.Second, "3m20s"},
		{time.Hour + 4*time.Minute, "1h04m"},
	}
	for _, tc := range cases {
		if got := formatDuration(tc.in); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripANSI(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[32mweb\x1b[0m", "web"},
		{"\x1b[38;2;255;136;0mweb\x1b[0m", "web"},
		{"\x1b[not-a-sequence", "\x1b[not-a-sequence"},
		{"trailing\x1b", "trailing\x1b"},
	}
	for _, tc := range cases {
		if got := stripANSI(tc.in); got != tc.want {
			t.Errorf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
