package console

import (
	"strings"

	"github.com/maborak/mabo-ctl/internal/ui"
)

// logRecord is one log line as received, classified once at ingest so a redraw
// never re-parses it.
type logRecord struct {
	// svc is the service the line came from; it labels lines in merged mode.
	svc string
	// text is the sanitized line.
	text string
	// lvl is ui.LogLevel(text): "error", "warn", "info", "debug" or "".
	lvl string
	// structured reports a complete JSON object or array, the lines pretty
	// JSON re-indents.
	structured bool
	// cont marks a continuation of the previous record's entry — an indented
	// line of a traceback or a wrapped message — so separators fall between
	// entries rather than between every line.
	cont bool

	// pretty caches the re-indented rows; prettyDone says whether it was
	// computed (a nil pretty after that means "not balanced, show raw").
	pretty     []string
	prettyDone bool
}

// newRecord classifies one line. prev is the record before it, or nil.
func newRecord(svc, text string, prev *logRecord) *logRecord {
	r := &logRecord{svc: svc, text: text, lvl: ui.LogLevel(text), structured: ui.IsJSONLine(text)}
	r.cont = prev != nil && prev.svc == svc && !r.structured && !prev.structured &&
		(text == "" || text[0] == ' ' || text[0] == '\t')
	return r
}

// maxPrettyBytes is the longest line pretty JSON re-indents. Indentation
// grows with nesting depth, so a long line is where the expansion is large;
// past this the line is shown as written.
const maxPrettyBytes = 64 << 10

// prettyRows returns the record's re-indented rows, or nil when it is not JSON,
// does not balance, nests too deeply, or is too long to expand safely. The
// cache is outside maxLogBytes, which is why both bounds exist: one line of
// 9000 nested brackets would otherwise expand to hundreds of megabytes.
func (r *logRecord) prettyRows() []string {
	if !r.structured || len(r.text) > maxPrettyBytes {
		return nil
	}
	if !r.prettyDone {
		r.prettyDone = true
		if s, ok := ui.PrettyJSON(r.text); ok {
			r.pretty = strings.Split(s, "\n")
		}
	}
	return r.pretty
}

// size is what a record costs against maxLogBytes. The cached pretty rows are
// not counted: they exist only for records drawn while pretty is on and are
// bounded by the same lines.
func (r *logRecord) size() int { return len(r.text) }

// logRow is one terminal row of the log pane.
type logRow struct {
	// svc labels the row in merged mode; first says the label is drawn on
	// this row rather than indented past.
	svc   string
	first bool
	text  string
	// sep marks a separator row between two entries.
	sep bool
}

// matches reports whether r passes the text filter and the level filter.
func (m Model) matches(r *logRecord, needle string) bool {
	if m.levelFilter != "" && r.lvl != m.levelFilter {
		return false
	}
	return needle == "" || strings.Contains(strings.ToLower(r.text), needle)
}

// shownRecords is what the pane draws from: the buffer, or while paused the
// snapshot taken when pause was pressed. The snapshot is its own slice, so
// lines evicted from the buffer while paused stay visible until resume: a
// paused pane that drained itself as output kept arriving would not be paused.
func (m Model) shownRecords() []*logRecord {
	if m.paused {
		return m.frozen
	}
	return m.recs
}

// rows expands the shown records into terminal rows: filtered, re-indented
// when pretty JSON is on, and split by separators between entries when those
// are on. Scrolling counts these rows, so a pretty-printed record scrolls
// like the several lines it occupies.
func (m Model) rows() []logRow {
	needle := strings.ToLower(m.filter)
	recs := m.shownRecords()
	out := make([]logRow, 0, len(recs))
	drawn := false
	for _, r := range recs {
		if !m.matches(r, needle) {
			continue
		}
		if m.separators && drawn && !r.cont {
			out = append(out, logRow{sep: true})
		}
		drawn = true
		lines := []string{r.text}
		if m.pretty {
			if p := r.prettyRows(); p != nil {
				lines = p
			}
		}
		for i, l := range lines {
			out = append(out, logRow{svc: r.svc, first: i == 0, text: l})
		}
	}
	return out
}

// appendRecord adds one line to the buffer, evicting the oldest records past
// maxLogLines or maxLogBytes, and keeps the viewport pinned while following.
func (m *Model) appendRecord(svc, text string) {
	var prev *logRecord
	if n := len(m.recs); n > 0 {
		prev = m.recs[n-1]
	}
	r := newRecord(svc, text, prev)
	m.recs = append(m.recs, r)
	m.recBytes += r.size()

	evict := 0
	for len(m.recs)-evict > maxLogLines || (m.recBytes > maxLogBytes && len(m.recs)-evict > 1) {
		m.recBytes -= m.recs[evict].size()
		m.recs[evict] = nil // release it now, not when append next reallocates
		evict++
	}
	if evict > 0 {
		m.recs = append(m.recs[:0], m.recs[evict:]...)
		if !m.follow && !m.paused {
			// Keep the reader roughly where they were. Rows are not records,
			// so this is an approximation; drawing clamps it.
			m.offset = max(0, m.offset-evict)
		}
	}
	if m.paused {
		m.pausedNew++
	}
	// Nothing else: a following pane is drawn from the bottom (viewOffset),
	// so an arriving line costs no pass over the buffer. Recomputing the
	// offset here made ingest O(buffer) per line.
}

// clearLog empties the buffer, like the web console's Clear.
func (m *Model) clearLog() {
	m.recs, m.recBytes = nil, 0
	m.frozen, m.pausedNew = nil, 0
	m.offset = 0
	m.follow = true
}

// togglePause freezes the pane on what it shows, or resumes it. Lines keep
// arriving while paused and count against the same caps, so a long pause
// never grows memory; the header says how many arrived.
func (m *Model) togglePause() {
	m.paused = !m.paused
	m.pausedNew = 0
	if m.paused {
		m.frozen = append([]*logRecord(nil), m.recs...)
		return
	}
	m.frozen = nil
	m.clampOffset()
}

// cycleLevel steps the level filter: all → error → warn → info → debug → all.
func (m *Model) cycleLevel() {
	next := ""
	if m.levelFilter == "" {
		next = ui.LogLevels[0]
	} else {
		for i, l := range ui.LogLevels {
			if l == m.levelFilter && i+1 < len(ui.LogLevels) {
				next = ui.LogLevels[i+1]
			}
		}
	}
	m.levelFilter = next
	m.follow = true
}
