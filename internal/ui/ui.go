// Package ui renders supervisor and service data as terminal text. It is pure:
// it reads no files, spawns nothing, and touches no global state, so every
// function here is a total function of its arguments and the [Renderer] it is
// called on.
//
// Two rules shape everything in this package.
//
// First, a status block is meant to be SCANNED, not read. Service labels are
// fixed width and carry a per-service colour, and the columns after them line
// up, so five services read as a table rather than five sentences. That is the
// main thing the tool is for.
//
// Second, colour is an ENHANCEMENT and never the only signal. NO_COLOR, a dumb
// terminal and a pipe are all real, and every one of them is honoured by [New].
// The readiness outcomes are therefore distinguished by a glyph AND a word
// before any colour is applied: a status block piped through `grep` or read by
// a colour-blind user carries exactly the same information as one on a colour
// terminal.
//
// [StatusJSON] is a separate, stable, machine-facing contract: no colour, no
// padding, no prose, and a fixed field order. Renaming one of its fields is a
// breaking change to anything parsing `mabo-ctl status --json`.
package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/maborak/mabo-ctl/internal/config"
	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// Renderer renders supervisor data for one output stream. The zero value is
// valid and renders plain, unlimited-width text; [New] configures one by
// inspecting the stream and the environment.
//
// A Renderer is not safe for concurrent use while [Renderer.SetServiceColors]
// or [Renderer.SetInstanceColors] is being called; once configured, the
// rendering methods only read it and may be called from several goroutines.
type Renderer struct {
	// Color reports whether ANSI colour is emitted. When false, output contains
	// no escape sequences at all — the glyph and the phase word still tell the
	// three readiness outcomes apart.
	Color bool
	// Width is the usable terminal width in columns, or 0 when it is unknown.
	// A non-zero Width truncates the free-form detail column so a long log line
	// cannot wrap and destroy the alignment of the block; it never truncates a
	// service name, phase, port or pid.
	Width int

	// colors maps service name to its declared colour. A service that is absent
	// or declares no colour gets a stable colour derived from its name.
	colors map[string]string
	// labelWidth is the width service labels are padded to outside a status
	// block, where there is no set of rows to measure. 0 means no padding.
	labelWidth int
}

// New returns a Renderer configured for out, which is normally os.Stdout.
//
// Colour is enabled only when all of the following hold, because every one of
// them is a real way for escape sequences to end up somewhere they cannot be
// interpreted:
//
//   - NO_COLOR is not set. Any value disables colour, including the empty
//     string; the variable's presence is the signal.
//   - TERM is neither "dumb" nor empty.
//   - out is a character device, i.e. a terminal rather than a pipe or a file.
//
// Width is taken from COLUMNS when it parses as a positive number, and is 0
// otherwise. Detecting the real width of a terminal needs an ioctl, which this
// package deliberately cannot make; a caller that knows better — the console,
// which is told the size by its event loop — assigns [Renderer.Width] directly.
//
// A nil out yields a plain, unlimited-width Renderer rather than a panic.
func New(out *os.File) *Renderer {
	r := &Renderer{
		Color: colorEnabled(isTerminal(out), os.LookupEnv),
		Width: envWidth(os.LookupEnv),
	}
	return r
}

// isTerminal reports whether f refers to a character device. A pipe, a regular
// file, a closed handle and a nil *os.File all report false.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// colorEnabled applies the colour policy to an already-determined terminal
// status. It is separated from [New] so the policy can be tested without a real
// terminal: lookup stands in for os.LookupEnv.
func colorEnabled(terminal bool, lookup func(string) (string, bool)) bool {
	if _, ok := lookup("NO_COLOR"); ok {
		return false
	}
	if term, _ := lookup("TERM"); term == "dumb" || term == "" {
		return false
	}
	return terminal
}

// envWidth reads COLUMNS and returns it, or 0 when it is absent, unparseable or
// not positive.
func envWidth(lookup func(string) (string, bool)) int {
	v, ok := lookup("COLUMNS")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// SetServiceColors records the declared colour of each service, keyed by
// service name, and fixes the label width used outside a status block to the
// longest of those names. A service with no entry, or an entry that is empty or
// unrecognised, keeps a stable colour derived from its name, so labels stay
// distinguishable even in a config that declares no colours at all.
//
// It copies colors; the caller may mutate the map afterwards.
func (r *Renderer) SetServiceColors(colors map[string]string) {
	r.colors = make(map[string]string, len(colors))
	r.labelWidth = 0
	for name, c := range colors {
		r.colors[name] = c
		if w := width(name); w > r.labelWidth {
			r.labelWidth = w
		}
	}
}

// SetInstanceColors is [Renderer.SetServiceColors] over resolved instances,
// which is where a caller normally has the declared colours to hand.
func (r *Renderer) SetInstanceColors(insts []service.Instance) {
	colors := make(map[string]string, len(insts))
	for _, in := range insts {
		colors[in.Name] = in.Color
	}
	r.SetServiceColors(colors)
}

// ServiceLabel renders name in that service's colour, padded to the width set
// by [Renderer.SetServiceColors] so labels from separate calls still line up.
// It is the label a streaming caller — a log pane, an event line — puts in
// front of text belonging to one service.
func (r *Renderer) ServiceLabel(name string) string {
	return pad(r.paint(r.serviceStyle(name), name), r.labelWidth)
}

// PhaseLabel returns the glyph and the word that identify p. Both are returned
// because colour is never the only signal: the glyph survives a colour-blind
// reader and the word survives a terminal that cannot draw the glyph, and the
// two together make the readiness outcomes distinguishable in a pipe.
//
// Every known phase has its own glyph and its own word. An unrecognised phase
// renders as "?" and the phase string itself, or "unknown" when it is empty —
// never as one of the real outcomes.
//
// The glyphs are chosen to be told apart by SHAPE, not only by colour or by
// weight: filled, half, hollow and diamond for the four live states, and a
// cross, a warning triangle and a slashed circle for the three that want a
// human. [supervisor.Phases] is the list every phase must appear in; a test in
// this package walks it, so a phase added without a glyph here fails there
// rather than reaching a user as "?".
func PhaseLabel(p supervisor.Phase) (glyph, word string) {
	switch p {
	case supervisor.PhaseReady:
		return "●", string(p)
	case supervisor.PhaseSlow:
		return "◐", string(p)
	case supervisor.PhaseStopped:
		return "○", string(p)
	case supervisor.PhaseFailed:
		return "✕", string(p)
	case supervisor.PhaseExited:
		return "⊘", string(p)
	case supervisor.PhaseDegraded:
		return "⚠", string(p)
	case supervisor.PhaseRunning:
		return "◆", string(p)
	case "":
		return "?", "unknown"
	default:
		return "?", string(p)
	}
}

// phaseCellWidth is the display width of the widest "<glyph> <word>" over every
// known phase. The status column is at least this wide even when no row needs
// it, so the columns after it land in the same place whatever the services
// happen to be doing: a block whose columns move between two invocations is a
// block the eye has to re-read instead of scan.
var phaseCellWidth = func() int {
	w := 0
	for _, p := range supervisor.Phases() {
		glyph, word := PhaseLabel(p)
		w = max(w, width(glyph)+1+width(word))
	}
	return w
}()

// phaseStyle is the colour a phase is drawn in. It only ever reinforces the
// glyph and the word, which carry the meaning on their own.
func phaseStyle(p supervisor.Phase) style {
	switch p {
	case supervisor.PhaseReady:
		return style{"32"}
	case supervisor.PhaseSlow:
		return style{"33"}
	case supervisor.PhaseDegraded:
		return style{"1;33"}
	case supervisor.PhaseStopped:
		return style{"2"}
	case supervisor.PhaseFailed:
		return style{"1;31"}
	case supervisor.PhaseExited:
		return style{"35"}
	case supervisor.PhaseRunning:
		return style{"36"}
	default:
		return style{"2"}
	}
}

// Column headings. They are part of the block so that a bare number under PID
// or PORT is never ambiguous.
const (
	hdrService = "SERVICE"
	hdrStatus  = "STATUS"
	hdrPort    = "PORT"
	hdrPID     = "PID"
	hdrProbe   = "PROBE"
	hdrDetail  = "DETAIL"
)

// colGap separates two columns.
const colGap = "  "

// missing is what an empty numeric column shows: a portless service, a stopped
// service with no pid, a service with no readiness probe and therefore no
// probe time.
const missing = "-"

// detailIndent prefixes the continuation lines of a multi-line detail, such as
// the log tail of a failed start.
const detailIndent = "    "

// minDetailWidth is the narrowest detail column worth printing. Below it, a
// truncated detail says nothing, so the detail is dropped from the row and the
// alignment of the columns that do fit is preserved.
const minDetailWidth = 12

// row is one status flattened to strings, ready to be measured and padded.
type row struct {
	name    string
	phase   supervisor.Phase
	glyph   string
	word    string
	port    string
	pid     string
	probe   string
	detail  []string // detail split on newlines; empty when there is none
	logPath string
}

// StatusBlock renders one line per status, in the order given — which is
// declaration order, so a service keeps its line between invocations and the
// eye learns where to look.
//
// Service names are padded to the longest name and coloured per service; the
// status, port, pid and probe columns are padded so they line up; the detail
// column is free-form and comes last, where a ragged right edge costs nothing.
// Each readiness outcome carries a glyph and a word, so the block is fully
// legible with colour disabled.
//
// A detail with more than one line — the log tail the supervisor attaches to a
// failed start — keeps its first line in the column and its remaining lines
// indented underneath, so the table never loses its alignment to a log line.
//
// The result has no trailing newline, and is empty for an empty slice.
func (r *Renderer) StatusBlock(sts []supervisor.Status) string {
	if len(sts) == 0 {
		return ""
	}

	rows := make([]row, 0, len(sts))
	for _, st := range sts {
		rows = append(rows, newRow(st))
	}

	wName := width(hdrService)
	wStatus := max(width(hdrStatus), phaseCellWidth)
	wPort := width(hdrPort)
	wPID := width(hdrPID)
	wProbe := width(hdrProbe)
	for _, rw := range rows {
		wName = max(wName, width(rw.name))
		wStatus = max(wStatus, width(rw.glyph)+1+width(rw.word))
		wPort = max(wPort, width(rw.port))
		wPID = max(wPID, width(rw.pid))
		wProbe = max(wProbe, width(rw.probe))
	}

	// Every column before the detail is fixed width, so the detail always starts
	// at the same offset and truncating it is the only width arithmetic needed.
	prefix := wName + wStatus + wPort + wPID + wProbe + 5*len(colGap)

	var b strings.Builder
	b.WriteString(r.header(wName, wStatus, wPort, wPID, wProbe))
	for _, rw := range rows {
		b.WriteString("\n")
		b.WriteString(r.line(rw, wName, wStatus, wPort, wPID, wProbe, prefix))
	}
	return b.String()
}

// header renders the column headings, dimmed when colour is on.
func (r *Renderer) header(wName, wStatus, wPort, wPID, wProbe int) string {
	cells := []string{
		pad(hdrService, wName),
		pad(hdrStatus, wStatus),
		pad(hdrPort, wPort),
		pad(hdrPID, wPID),
		pad(hdrProbe, wProbe),
		hdrDetail,
	}
	return r.paint(style{"2"}, strings.Join(cells, colGap))
}

// line renders one row plus any continuation lines its detail needs. prefix is
// the display width of everything before the detail column, used to decide how
// much of the detail fits in Width.
func (r *Renderer) line(rw row, wName, wStatus, wPort, wPID, wProbe, prefix int) string {
	status := r.paint(phaseStyle(rw.phase), rw.glyph+" "+rw.word)
	cells := []string{
		pad(r.paint(r.serviceStyle(rw.name), rw.name), wName),
		pad(status, wStatus),
		pad(rw.port, wPort),
		pad(rw.pid, wPID),
		pad(rw.probe, wProbe),
	}
	line := strings.Join(cells, colGap)

	if len(rw.detail) == 0 {
		return strings.TrimRight(line, " ")
	}

	avail := r.detailWidth(prefix)
	head := truncate(rw.detail[0], avail)
	line = strings.TrimRight(line+colGap+r.paint(style{"2"}, head), " ")
	if len(rw.detail) == 1 {
		return line
	}

	var b strings.Builder
	b.WriteString(line)
	for _, extra := range rw.detail[1:] {
		b.WriteString("\n")
		b.WriteString(detailIndent)
		b.WriteString(r.paint(style{"2"}, truncate(extra, r.detailWidth(len(detailIndent)))))
	}
	return b.String()
}

// detailWidth returns how many columns the detail may use after prefix, or 0
// when Width is unknown, in which case the detail is not truncated at all.
func (r *Renderer) detailWidth(prefix int) int {
	if r.Width <= 0 {
		return 0
	}
	avail := r.Width - prefix - len(colGap)
	if avail < minDetailWidth {
		return minDetailWidth
	}
	return avail
}

// newRow flattens a Status into printable cells. A zero port, pid or elapsed is
// rendered as "-" rather than as 0, which would read as a real value.
//
// The elapsed cell is headed PROBE, not UPTIME: [supervisor.Status.Elapsed] is
// how long the last readiness probe took, not how long the process has been
// running. A portless service shows "-" there because it is never probed, which
// under an UPTIME heading would claim a running process had been up for no time
// at all.
func newRow(st supervisor.Status) row {
	glyph, word := PhaseLabel(st.Phase)
	rw := row{
		name:    st.Name,
		phase:   st.Phase,
		glyph:   glyph,
		word:    word,
		port:    missing,
		pid:     missing,
		probe:   formatDuration(st.Elapsed),
		logPath: st.LogPath,
	}
	if st.Port > 0 {
		rw.port = strconv.Itoa(st.Port)
	}
	if st.PID > 0 {
		rw.pid = strconv.Itoa(st.PID)
	}
	rw.detail = detailLines(st)
	return rw
}

// detailLines builds the detail column: the supervisor's own Detail if it set
// one, otherwise whatever the probe result can say. The HTTP status is worth
// showing next to a ready service, and a slow service is more useful with the
// URL it is still waiting on than with nothing.
func detailLines(st supervisor.Status) []string {
	detail := strings.TrimRight(st.Detail, "\n")
	if detail == "" {
		switch {
		case st.HTTP != 0:
			detail = fmt.Sprintf("HTTP %d", st.HTTP)
		case st.Phase == supervisor.PhaseSlow && st.Health != "":
			detail = "waiting for " + st.Health
		case st.Phase == supervisor.PhaseDegraded && st.Health != "":
			detail = "not answering " + st.Health
		default:
			return nil
		}
	} else if st.HTTP != 0 {
		detail = fmt.Sprintf("HTTP %d %s", st.HTTP, detail)
	}
	lines := strings.Split(detail, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	return lines
}

// Event renders one supervisor event as a single line: the service label, the
// phase glyph and word, and the message. An event carrying an error appends it,
// because an event whose error is dropped is exactly the silent failure this
// tool exists to stop reporting.
//
// The label and the phase are padded to fixed widths, so a stream of events
// from several services reads as a column rather than as ragged prose.
//
// An event with no service — a global one, such as a reset — renders without a
// label. The result has no trailing newline, and is empty when the event says
// nothing at all.
func (r *Renderer) Event(e supervisor.Event) string {
	var parts []string
	if e.Service != "" {
		parts = append(parts, r.ServiceLabel(e.Service))
	}
	if e.Phase != "" {
		glyph, word := PhaseLabel(e.Phase)
		parts = append(parts, pad(r.paint(phaseStyle(e.Phase), glyph+" "+word), phaseCellWidth))
	}
	if e.Msg != "" {
		parts = append(parts, e.Msg)
	}
	// Producers commonly build an Event as {Msg: err.Error(), Err: err} so that
	// a consumer reading either field alone still sees the reason. Rendering
	// both then prints the same sentence twice on one line, so append the error
	// only when it actually says something the message did not.
	if e.Err != nil && !strings.Contains(e.Msg, e.Err.Error()) {
		parts = append(parts, r.paint(style{"31"}, "error: "+e.Err.Error()))
	}
	return strings.TrimRight(strings.Join(parts, colGap), " ")
}

// PortOrigins reports ONLY the ports that a persisted .dev/run.env value is
// holding away from the value mabo-ctl.yaml now declares, and says how to clear
// it. It returns "" — printing nothing — when no port is overridden, which is
// the normal case.
//
// This exists because the opposite behaviour cost a real debugging round:
// changing a default port in the config appeared to do nothing, because a
// persisted port silently outranked it. Stale state winning is defensible;
// stale state winning invisibly is not.
//
// The result has no trailing newline.
func (r *Renderer) PortOrigins(origins []service.Origin) string {
	var lines []string
	for _, o := range origins {
		if !o.Override {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %s uses port %d from %s, not the declared %d",
			r.paint(style{"1;33"}, "port override:"),
			r.paint(r.serviceStyle(o.Service), o.Service),
			o.Port, o.Source, o.Declared))
	}
	if len(lines) == 0 {
		return ""
	}
	lines = append(lines, r.paint(style{"2"},
		"clear the persisted ports with `mabo-ctl reset`, or delete .dev/run.env"))
	return strings.Join(lines, "\n")
}

// Error renders err for a human.
//
// A *config.ValidationError becomes a bulleted list with one bullet per
// problem, never a single squashed line: a config with six mistakes should
// print six bullets so all six can be fixed in one pass. The same treatment is
// given to any error exposing Messages() []string, and to a joined error, so a
// multi-problem error from another package is not flattened either.
//
// A nil error renders as "".
func (r *Renderer) Error(err error) string {
	if err == nil {
		return ""
	}

	head, msgs := explode(err)
	if len(msgs) == 0 {
		return r.paint(style{"1;31"}, "error:") + " " + head
	}

	var b strings.Builder
	b.WriteString(r.paint(style{"1;31"}, "error:"))
	b.WriteString(" ")
	b.WriteString(head)
	for _, m := range msgs {
		b.WriteString("\n  ")
		b.WriteString(r.paint(style{"31"}, "•"))
		b.WriteString(" ")
		b.WriteString(m)
	}
	return b.String()
}

// explode splits an error into a headline and a bullet list. It returns no
// bullets for an ordinary error, in which case the headline is the whole
// message.
func explode(err error) (head string, msgs []string) {
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		return validationHead(ve), ve.Messages()
	}

	var lister interface{ Messages() []string }
	if errors.As(err, &lister) {
		if m := lister.Messages(); len(m) > 0 {
			return err.Error(), m
		}
	}

	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		errs := joined.Unwrap()
		if len(errs) > 1 {
			msgs = make([]string, 0, len(errs))
			for _, e := range errs {
				msgs = append(msgs, e.Error())
			}
			return "several problems:", msgs
		}
	}

	return err.Error(), nil
}

// validationHead is the one-line summary above a validation bullet list. It
// names the file, because mabo-ctl discovers its config by walking up and the
// file it found may not be the one the user was thinking of.
func validationHead(ve *config.ValidationError) string {
	noun := "problems"
	if len(ve.Problems) == 1 {
		noun = "problem"
	}
	path := ve.Path
	if path == "" {
		path = config.FileName
	}
	return fmt.Sprintf("%s is invalid (%d %s):", path, len(ve.Problems), noun)
}

// statusRecord is the wire form of a [supervisor.Status].
//
// THIS IS A STABLE CONTRACT. `mabo-ctl status --json` is the documented
// integration point, so these field names and this field order are part of the
// tool's public interface: renaming, reordering or removing one breaks every
// script parsing it. Adding a field at the end is the only backwards-compatible
// change. A golden test in this package fails loudly on any of the others.
//
// The PHASE VOCABULARY is part of the contract too, and it is the one part that
// has changed: `degraded` and `exited` joined it, and anything switching on
// phase has to handle them. Both describe states that were previously reported
// as something else — `exited` was reported as `stopped`, which made a service
// that crashed indistinguishable from one that was never started, and
// `degraded` was reported as `slow` for as long as the service stayed broken.
type statusRecord struct {
	Service   string `json:"service"`
	Phase     string `json:"phase"`
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	Health    string `json:"health"`
	HTTP      int    `json:"http_status"`
	Detail    string `json:"detail"`
	LogPath   string `json:"log_path"`
	ElapsedMS int64  `json:"elapsed_ms"`
	// StartedAt is RFC 3339, or "" when mabo-ctl does not know when — or whether
	// — the service was started. A timestamp is the right shape here rather
	// than a duration: it does not go stale in a file a consumer saved.
	StartedAt string `json:"started_at"`
	// UptimeMS is how long the live process has been up, 0 when nothing is
	// running. It is a duration rather than a second timestamp because it is
	// the number a dashboard prints, and computing it from StartedAt would make
	// every consumer re-implement the "unknown spawn time" case.
	UptimeMS int64 `json:"uptime_ms"`
	// ExitCode is the last observed exit status, -1 when the process was
	// signalled or the status was never seen. It is meaningful only when
	// ExitedAt is non-empty: 0 means both "exited cleanly" and "never died".
	ExitCode int `json:"exit_code"`
	// ExitSignal names the signal that killed the process ("SIGKILL"), "" when
	// none did.
	ExitSignal string `json:"exit_signal"`
	// ExitedAt is RFC 3339, or "" when mabo-ctl has not seen this service die.
	// It is the field to test before trusting ExitCode or ExitSignal.
	ExitedAt string `json:"exited_at"`
}

// timestamp renders t for the wire, and the zero time as "".
//
// A zero time.Time marshals to "0001-01-01T00:00:00Z", which parses fine and
// means nothing: a consumer would have to know to compare against year 1 to
// discover that mabo-ctl simply does not know. An empty string cannot be mistaken
// for a date.
func timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// StatusJSON renders statuses as the stable machine contract behind
// `mabo-ctl status --json`: a JSON array of objects, one per status, in the order
// given. It carries no colour, no padding and no human prose — every value is
// the raw one from [supervisor.Status], with the duration expressed as whole
// milliseconds so no consumer has to parse a Go duration string.
//
// Field order is fixed and deterministic. HTML escaping is off, so a health URL
// containing & or ? survives verbatim. An empty slice renders as "[]", never
// "null", so a consumer can iterate the result unconditionally. The result has
// no trailing newline.
//
// It returns an error only if a status contains a value the JSON encoder
// rejects, which the current field types make impossible; the error is
// propagated rather than swallowed so that stays true if the types change.
func StatusJSON(sts []supervisor.Status) ([]byte, error) {
	records := make([]statusRecord, 0, len(sts))
	for _, st := range sts {
		records = append(records, statusRecord{
			Service:    st.Name,
			Phase:      string(st.Phase),
			PID:        st.PID,
			Port:       st.Port,
			Health:     st.Health,
			HTTP:       st.HTTP,
			Detail:     st.Detail,
			LogPath:    st.LogPath,
			ElapsedMS:  st.Elapsed.Milliseconds(),
			StartedAt:  timestamp(st.StartedAt),
			UptimeMS:   st.Uptime.Milliseconds(),
			ExitCode:   st.ExitCode,
			ExitSignal: st.ExitSignal,
			ExitedAt:   timestamp(st.ExitedAt),
		})
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(records); err != nil {
		return nil, fmt.Errorf("ui: encoding status JSON: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// style is a set of ANSI SGR parameters, e.g. "1;31" for bold red. Colours are
// written by hand rather than delegated to a styling library so that this
// package has no global colour-profile state to be surprised by: what [Renderer]
// decides is exactly what is emitted.
type style struct{ params string }

// paint wraps text in s when the Renderer has colour enabled, and returns text
// unchanged otherwise. Padding is applied by the caller AFTER painting, so an
// escape sequence never wraps trailing spaces.
func (r *Renderer) paint(s style, text string) string {
	if !r.Color || text == "" || s.params == "" {
		return text
	}
	return "\x1b[" + s.params + "m" + text + "\x1b[0m"
}

// fallbackPalette gives a service with no declared colour a stable one. Two
// services rarely collide, and even when they do the fixed-width label still
// tells them apart — colour is never the only signal.
var fallbackPalette = []string{"cyan", "green", "yellow", "blue", "magenta", "bright-cyan", "bright-green"}

// serviceStyle is the colour of a service's label: the one declared in
// mabo-ctl.yaml when it is recognised, otherwise a stable choice derived from the
// name so the same service is the same colour in every run.
func (r *Renderer) serviceStyle(name string) style {
	if c, ok := r.colors[name]; ok {
		if s, ok := parseColor(c); ok {
			return s
		}
	}
	h := fnv.New32a()
	// Hash.Write never returns an error; the interface keeps the signature.
	_, _ = h.Write([]byte(name))
	s, _ := parseColor(fallbackPalette[int(h.Sum32()%uint32(len(fallbackPalette)))])
	return s
}

// namedColors maps the colour names mabo-ctl.yaml may use to SGR foreground
// parameters.
var namedColors = map[string]string{
	"black":   "30",
	"red":     "31",
	"green":   "32",
	"yellow":  "33",
	"blue":    "34",
	"magenta": "35",
	"cyan":    "36",
	"white":   "37",
	"gray":    "90",
	"grey":    "90",
}

// parseColor turns a declared colour into SGR parameters. It accepts a name
// from namedColors, the same name prefixed "bright-" or "bright", a 0..255
// number for the 256-colour palette, and #rrggbb for a true-colour terminal.
// It reports false for anything else, including "", so the caller can fall back
// rather than emit a broken escape sequence.
func parseColor(c string) (style, bool) {
	c = strings.ToLower(strings.TrimSpace(c))
	if c == "" {
		return style{}, false
	}

	bright := false
	for _, p := range []string{"bright-", "bright_", "bright "} {
		if rest, ok := strings.CutPrefix(c, p); ok {
			c, bright = rest, true
			break
		}
	}

	if code, ok := namedColors[c]; ok {
		n, err := strconv.Atoi(code)
		if err != nil {
			return style{}, false
		}
		if bright && n < 90 {
			n += 60
		}
		return style{strconv.Itoa(n)}, true
	}

	if hex, ok := strings.CutPrefix(c, "#"); ok && len(hex) == 6 {
		v, err := strconv.ParseUint(hex, 16, 32)
		if err != nil {
			return style{}, false
		}
		return style{fmt.Sprintf("38;2;%d;%d;%d", v>>16&0xff, v>>8&0xff, v&0xff)}, true
	}

	if n, err := strconv.Atoi(c); err == nil && n >= 0 && n <= 255 {
		return style{"38;5;" + strconv.Itoa(n)}, true
	}

	return style{}, false
}

// width is the display width of s in columns. Service names, ports and pids are
// ASCII by construction — config rejects a name that is not — and the status
// glyphs occupy one column, so counting runes is exact for everything this
// package aligns.
func width(s string) int { return utf8.RuneCountInString(stripANSI(s)) }

// pad right-pads s to w columns, measuring the visible text so a painted cell
// pads to the same width as a plain one. It never truncates: a cell wider than
// the column would mean the column was mis-measured, and losing a character is
// worse than losing the alignment of one row.
func pad(s string, w int) string {
	if n := w - width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// truncate shortens s to at most w columns, marking the cut with an ellipsis. A
// w of 0 or less means "no limit". s is expected to be unpainted text.
func truncate(s string, w int) string {
	if w <= 0 || utf8.RuneCountInString(s) <= w {
		return s
	}
	runes := []rune(s)
	if w == 1 {
		return "…"
	}
	return string(runes[:w-1]) + "…"
}

// stripANSI removes SGR sequences so painted text can be measured. It handles
// exactly the sequences this package emits — CSI ... m — and leaves anything
// else alone.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// formatDuration renders an elapsed time compactly enough for a fixed column:
// "-" for none, "820ms", "1.2s", "3m20s", "1h04m". Precision drops as the
// magnitude rises because nobody reads milliseconds off an hour-old process.
func formatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return missing
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}
