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
		`terminals.recoverOutput();`,
		`older output may be omitted. The shell keeps running.`,
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("terminal output backpressure lost %q", want)
		}
	}
}
