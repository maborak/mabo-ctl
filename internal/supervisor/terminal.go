package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// terminalScrollback is how much recent output a terminal keeps for a client
// that attaches late or reattaches after a reload. It is a byte ring: enough
// for a full-screen program to repaint from, small enough that a handful of
// terminals cost nothing.
const terminalScrollback = 1 << 20

// terminalSubBuffer is how many output chunks one attached client may fall
// behind before it is dropped. A chunk is at most the 32 KiB read buffer, so
// this bounds a stalled client's queue at 2 MiB. A dropped client reconnects
// and replays the scrollback, which is better than blocking the pty — and with
// it the shell — on a slow browser.
const terminalSubBuffer = 64

// terminalHangupGrace is how long Close waits after SIGHUP before SIGKILL.
const terminalHangupGrace = 2 * time.Second

// terminalDrain is how long a terminal whose shell has exited keeps reading
// its pty for the last output. A process that left the shell's session (a
// setsid'd daemon started from it) can hold the pty open indefinitely, so the
// end of a terminal is the shell's exit, never the pty's end of file.
const terminalDrain = 500 * time.Millisecond

// MaxTerminals bounds how many shells may be open at once.
const MaxTerminals = 16

// ErrNoTerminal reports a terminal id that is not open.
var ErrNoTerminal = errors.New("no such terminal")

// ErrTooManyTerminals reports an Open refused because MaxTerminals are open.
var ErrTooManyTerminals = fmt.Errorf("at most %d terminals may be open at once", MaxTerminals)

// Terminals owns the interactive shells the web console's Terminal tab runs.
//
// Each terminal is a login shell on its own pty, in its own session, started
// in the repository root. It outlives the browser tab that opened it, keeps a
// scrollback ring for whoever attaches next, and ends when the shell exits,
// when it is closed, or when [Terminals.CloseAll] runs as the console stops.
type Terminals struct {
	dir   string
	shell string

	mu      sync.Mutex
	seq     int
	pending int // slots reserved by an Open whose pty is still starting
	open    map[string]*Terminal
}

// NewTerminals returns a terminal set whose shells start in dir. The shell is
// $SHELL, falling back to /bin/sh.
func NewTerminals(dir string) *Terminals {
	shell := os.Getenv("SHELL")
	if shell == "" || !filepath.IsAbs(shell) {
		shell = "/bin/sh"
	}
	return &Terminals{dir: dir, shell: shell, open: map[string]*Terminal{}}
}

// TerminalInfo describes one open terminal.
type TerminalInfo struct {
	// ID names the terminal in every route that addresses it.
	ID string
	// PID is the shell's process id.
	PID int
	// StartedAt is when the shell was spawned.
	StartedAt time.Time
}

// Terminal is one shell on a pty.
type Terminal struct {
	info TerminalInfo
	cmd  *exec.Cmd
	pty  *os.File

	mu     sync.Mutex
	ring   []byte
	subs   map[chan []byte]struct{}
	done   chan struct{}
	exited bool
}

// Open spawns a new shell on a pty sized cols×rows.
func (t *Terminals) Open(cols, rows int) (TerminalInfo, error) {
	// Reserve the slot before the spawn: a second Open that arrived while this
	// one was starting would otherwise see the same count and both would pass.
	t.mu.Lock()
	if len(t.open)+t.pending >= MaxTerminals {
		t.mu.Unlock()
		return TerminalInfo{}, ErrTooManyTerminals
	}
	t.pending++
	t.mu.Unlock()

	cmd := exec.Command(t.shell, "-l")
	cmd.Dir = t.dir
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"MABO_CTL_TERMINAL=1",
	)
	f, err := pty.StartWithSize(cmd, winsize(cols, rows))
	if err != nil {
		t.mu.Lock()
		t.pending--
		t.mu.Unlock()
		return TerminalInfo{}, fmt.Errorf("starting %s on a pty: %w", t.shell, err)
	}

	t.mu.Lock()
	t.pending--
	t.seq++
	id := strconv.Itoa(t.seq)
	term := &Terminal{
		info: TerminalInfo{
			ID:        id,
			PID:       cmd.Process.Pid,
			StartedAt: time.Now(),
		},
		cmd:  cmd,
		pty:  f,
		subs: map[chan []byte]struct{}{},
		done: make(chan struct{}),
	}
	t.open[id] = term
	t.mu.Unlock()

	pumped := make(chan struct{})
	go func() { term.pump(); close(pumped) }()
	go func() {
		term.reap(pumped)
		t.mu.Lock()
		delete(t.open, id)
		t.mu.Unlock()
	}()
	return term.info, nil
}

// List returns the open terminals, oldest first.
func (t *Terminals) List() []TerminalInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]TerminalInfo, 0, len(t.open))
	for i := 1; i <= t.seq; i++ {
		if term, ok := t.open[strconv.Itoa(i)]; ok {
			out = append(out, term.info)
		}
	}
	return out
}

// Get returns the open terminal named id.
func (t *Terminals) Get(id string) (*Terminal, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	term, ok := t.open[id]
	if !ok {
		return nil, ErrNoTerminal
	}
	return term, nil
}

// Close hangs up the terminal named id and waits for its shell to go.
func (t *Terminals) Close(id string) error {
	term, err := t.Get(id)
	if err != nil {
		return err
	}
	term.hangup()
	return nil
}

// CloseAll hangs up every open terminal and waits for each to end.
func (t *Terminals) CloseAll() {
	t.mu.Lock()
	all := make([]*Terminal, 0, len(t.open))
	for _, term := range t.open {
		all = append(all, term)
	}
	t.mu.Unlock()

	var wg sync.WaitGroup
	for _, term := range all {
		wg.Add(1)
		go func() { defer wg.Done(); term.hangup() }()
	}
	wg.Wait()
}

// Info describes the terminal.
func (term *Terminal) Info() TerminalInfo { return term.info }

// Write sends input to the shell as if it were typed.
func (term *Terminal) Write(p []byte) error {
	_, err := term.pty.Write(p)
	return err
}

// Resize tells the shell its window is now cols×rows.
func (term *Terminal) Resize(cols, rows int) error {
	return pty.Setsize(term.pty, winsize(cols, rows))
}

// Attach returns the scrollback so far and a channel of everything written
// after it, with no gap and no overlap between the two. The channel is closed
// when the terminal ends or when this client falls too far behind; detach must
// be called when the client goes away.
func (term *Terminal) Attach() (backlog []byte, out <-chan []byte, detach func()) {
	ch := make(chan []byte, terminalSubBuffer)
	term.mu.Lock()
	backlog = term.backlog()
	if term.exited {
		close(ch)
	} else {
		term.subs[ch] = struct{}{}
	}
	term.mu.Unlock()
	return backlog, ch, func() {
		term.mu.Lock()
		if _, ok := term.subs[ch]; ok {
			delete(term.subs, ch)
			close(ch)
		}
		term.mu.Unlock()
	}
}

// Done is closed when the shell has exited.
func (term *Terminal) Done() <-chan struct{} { return term.done }

// pump copies pty output into the ring and out to every attached client until
// the pty can no longer be read.
func (term *Terminal) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := term.pty.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			term.mu.Lock()
			term.ring = append(term.ring, chunk...)
			// Trimmed at twice the cap, to the cap: copying the whole ring on
			// every read would make a chatty program cost a megabyte per chunk.
			if len(term.ring) > 2*terminalScrollback {
				term.ring = append([]byte(nil), term.ring[len(term.ring)-terminalScrollback:]...)
			}
			for ch := range term.subs {
				select {
				case ch <- chunk:
				default:
					delete(term.subs, ch)
					close(ch)
				}
			}
			term.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// reap waits for the shell, lets pump drain what it wrote last, then ends the
// terminal. Done is closed before the clients' channels, so a client that sees
// its channel close can tell an ended terminal from being dropped for lag.
func (term *Terminal) reap(pumped <-chan struct{}) {
	_ = term.cmd.Wait()
	select {
	case <-pumped:
	case <-time.After(terminalDrain):
	}
	_ = term.pty.Close()

	term.mu.Lock()
	term.exited = true
	close(term.done)
	for ch := range term.subs {
		delete(term.subs, ch)
		close(ch)
	}
	term.mu.Unlock()
}

// backlog copies the last terminalScrollback bytes of output. The caller holds
// term.mu.
func (term *Terminal) backlog() []byte {
	if over := len(term.ring) - terminalScrollback; over > 0 {
		return append([]byte(nil), term.ring[over:]...)
	}
	return append([]byte(nil), term.ring...)
}

// hangup ends the shell the way closing a terminal window does: SIGHUP to its
// whole session, then SIGKILL if it is still there after the grace. It returns
// once the shell has been reaped.
func (term *Terminal) hangup() {
	// A shell that already exited has been reaped, and its pid may belong to
	// someone else by now.
	select {
	case <-term.done:
		return
	default:
	}
	pid := term.info.PID
	_ = signalGroup(pid, syscall.SIGHUP)
	select {
	case <-term.done:
		return
	case <-time.After(terminalHangupGrace):
	}
	_ = signalGroup(pid, killSignal)
	<-term.done
}

// winsize clamps a requested size to something a pty accepts.
func winsize(cols, rows int) *pty.Winsize {
	clamp := func(v, lo, hi int) uint16 {
		if v < lo {
			v = lo
		}
		if v > hi {
			v = hi
		}
		return uint16(v)
	}
	return &pty.Winsize{Cols: clamp(cols, 2, 1000), Rows: clamp(rows, 2, 500)}
}
