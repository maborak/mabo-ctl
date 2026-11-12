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

// prettyRows returns the record's re-indented rows, or nil when it is not JSON
// or does not balance.
func (r *logRecord) prettyRows() []string {
	if !r.structured {
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

// shownRecords is the slice of the buffer the pane draws from: all of it, or
// while paused only what had arrived when pause was pressed.
func (m Model) shownRecords() []*logRecord {
	if m.paused {
		return m.recs[:min(m.pauseEnd, len(m.recs))]
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
		m.pauseEnd = max(0, m.pauseEnd-evict)
		if !m.follow {
			// Keep the reader roughly where they were. Rows are not records, so
			// this is an approximation; the clamp below keeps it in range.
			m.offset = max(0, m.offset-evict)
		}
	}
	if m.paused {
		m.pausedNew++
		return
	}
	if m.follow {
		m.offset = m.bottomOffset()
	} else {
		m.clampOffset()
	}
}

// clearLog empties the buffer, like the web console's Clear.
func (m *Model) clearLog() {
	m.recs, m.recBytes = nil, 0
	m.pauseEnd, m.pausedNew = 0, 0
	m.offset = 0
	m.follow = true
}

// togglePause freezes the pane on what it shows, or resumes it. Lines keep
// arriving while paused and count against the same caps, so a long pause
// never grows memory; the header says how many arrived.
func (m *Model) togglePause() {
	m.paused = !m.paused
	if m.paused {
		m.pauseEnd = len(m.recs)
		m.pausedNew = 0
		return
	}
	m.pausedNew = 0
	if m.follow {
		m.offset = m.bottomOffset()
	} else {
		m.clampOffset()
	}
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
	m.offset = m.bottomOffset()
}
