package web

import (
	"strings"
	"testing"
)

// TestConsoleLogRetentionBoundsEveryQueue guards the suspended-rAF failure:
// limiting history alone must not leave an unbounded queue awaiting a paint.
func TestConsoleLogRetentionBoundsEveryQueue(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		`var MAX_LOG_CHARS    = 2 * 1024 * 1024;`,
		`var MAX_LOG_LINE_CHARS = 16 * 1024;`,
		`this.bufferChars += rec.text.length;`,
		`this.buf.length > MAX_LINES || this.bufferChars > MAX_LOG_CHARS`,
		`this.bufferChars -= evicted.text.length;`,
		`if (this.pending[0] === evicted) { this.pending.shift(); }`,
		`text: logPreview(text)`, `var sev = detectLevel(text);`,
		`JSON.parse(JSON.stringify(text.slice(0, end)))`,
		`display truncated; full line remains in the log file`,
		`node.dataset.seq = String(rec.seq);`,
		`Number(lines[0].dataset.seq) < first`,
		`if (this.paused) { return; }`,
		`if (this.frame) { cancelAnimationFrame(this.frame); this.frame = 0; }`,
		`older lines discarded from display`,
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console memory budget lost %q", want)
		}
	}
	if strings.Count(consoleHTML, `this.resetBuffer();`) != 3 {
		t.Error("single-service bind, merged bind and Clear must reset retained memory accounting")
	}
}

// TestConsoleTerminalOutputHasBackpressure guards the queue before xterm has
// parsed output: its finite scrollback does not bound asynchronous writes.
func TestConsoleTerminalOutputHasBackpressure(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		`var TERM_OUTPUT_LIMIT = 2 * 1024 * 1024;`,
		`s.pendingOutput + data.byteLength > TERM_OUTPUT_LIMIT`,
		`s.pendingOutput += data.byteLength;`,
		`s.term.write(data, function () {`,
		`s.pendingOutput -= data.byteLength;`,
		`if (replay) { s.replaying++; }`,
		`if (replay) { s.replaying--; }`,
		`s.es.onopen = s.es.onmessage = s.es.onerror = null;`,
		`s.outputPaused = true;`,
		`document.visibilityState !== "visible"`,
		`currentTab !== "terminal" || this.active !== s.id`,
		`"shell " + s.id + ": display paused`,
		`"shell " + s.id + ": display recovered`,
		`older output may be omitted. The shell keeps running.`,
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("terminal output backpressure lost %q", want)
		}
	}
}

// TestConsoleTerminalOutputRecoversWithoutAClick guards WHERE recovery runs,
// not merely that the call exists: resumeOutput refuses while the page is
// hidden or another console tab is open, so a display that drained then must
// be resumed by the events that end those states. A call made only at boot,
// before any shell exists, recovers nothing.
func TestConsoleTerminalOutputRecoversWithoutAClick(t *testing.T) {
	t.Parallel()
	for _, site := range []struct{ name, start, end string }{
		{"returning to a hidden page (recoverStreams)", "function recoverStreams() {", "\n  }\n"},
		{"switching to the Terminal tab (terminals.show)", "    show: function () {", "\n    },\n"},
	} {
		body := jsBlock(t, site.start, site.end)
		if !strings.Contains(body, "recoverOutput()") {
			t.Errorf("%s no longer resumes a paused terminal display:\n%s", site.name, body)
		}
	}
	vis := jsBlock(t, "document.addEventListener(\"visibilitychange\", function () {\n    if (document.visibilityState === \"visible\") {", "\n  });\n")
	if !strings.Contains(vis, "recoverStreams()") {
		t.Errorf("visibilitychange no longer reaches recoverStreams:\n%s", vis)
	}
}

// jsBlock returns the console source from start through the first end after it.
func jsBlock(t *testing.T, start, end string) string {
	t.Helper()
	i := strings.Index(consoleHTML, start)
	if i < 0 {
		t.Fatalf("console no longer contains %q", start)
	}
	j := strings.Index(consoleHTML[i:], end)
	if j < 0 {
		t.Fatalf("no %q after %q", end, start)
	}
	return consoleHTML[i : i+j+len(end)]
}
