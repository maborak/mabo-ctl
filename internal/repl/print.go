package repl

import (
	"fmt"
	"io"
	"sync"
)

// eraseLine returns the cursor to column zero and clears to the end of the
// line. It is the whole of this package's terminal control: enough to lift a
// prompt out of the way of an asynchronous notice and put it back, and nothing
// more. A REPL that redrew more than that would need the raw mode and the
// screen model this package deliberately does not have.
const eraseLine = "\r\x1b[K"

// printer serialises every write to the output stream and knows whether a
// prompt is currently sitting on the last line.
//
// It exists because two goroutines write here: the loop, which prints the
// prompt and each command's result, and the crash watcher, which prints
// `api exited (code 1)` whenever it notices one. Without the prompt bookkeeping
// the second one lands on top of the first, leaving the user staring at a line
// that is half notice and half prompt with no way to tell where their own
// typing starts.
type printer struct {
	mu sync.Mutex
	w  io.Writer
	// redraw enables the erase-and-reprint dance around an asynchronous line.
	// It is off for a pipe and for a test, where there is no cursor to move and
	// an escape sequence would be noise in the captured output.
	redraw bool
	// prompt is the text last written by showPrompt.
	prompt string
	// shown reports that prompt is on screen and has not been closed off by the
	// user pressing Enter.
	shown bool
}

// line writes s followed by a newline. It is for output the loop produces while
// no prompt is on screen.
func (p *printer) line(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(p.w, s)
}

// showPrompt writes the prompt and records that it is on screen.
func (p *printer) showPrompt(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prompt, p.shown = s, true
	fmt.Fprint(p.w, s)
}

// promptEntered records that the user pressed Enter, so the terminal has
// already echoed the newline that closed the prompt line.
func (p *printer) promptEntered() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shown = false
}

// promptEOF closes off a prompt that ended without a newline — Ctrl-D, or a
// stream that simply stopped — by writing the newline the terminal never
// echoed. Without it the shell's own prompt would resume on mabo-ctl's.
func (p *printer) promptEOF() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shown {
		fmt.Fprintln(p.w)
		p.shown = false
	}
}

// notice writes s as its own line without corrupting a prompt the user may be
// typing into.
//
// On a terminal the prompt line is erased, the notice is written, and the
// prompt is drawn again. Text the user had already typed is NOT restored: this
// package has no readline and therefore no record of it. The tty keeps it in
// its own line buffer, so pressing Enter still submits exactly what was typed —
// it is invisible for the moment between the notice and the next keystroke,
// which is the honest cost of not shipping a line editor.
func (p *printer) notice(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shown && p.redraw {
		fmt.Fprint(p.w, eraseLine)
	}
	fmt.Fprintln(p.w, s)
	if p.shown && p.redraw {
		fmt.Fprint(p.w, p.prompt)
	}
}
