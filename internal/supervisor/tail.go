package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// failLogLines is how much of a dead service's log to quote back.
	failLogLines = 20

	// deathPollInterval is how often awaitDeath re-checks a signalled pid.
	deathPollInterval = 50 * time.Millisecond

	// reapGrace is how long an orphan reaped by port gets to exit after SIGTERM
	// before reset escalates. It is shorter than stop_grace: this process is not
	// one mabo-ctl started, so there is no graceful-shutdown contract to honour,
	// and the user asked for the port back.
	reapGrace = 3 * time.Second

	// killGrace is how long to wait for a process to disappear after SIGKILL.
	// SIGKILL cannot be caught, so this only ever covers the kernel finishing
	// teardown; a process still present after it is stuck in uninterruptible
	// sleep and no signal will help.
	killGrace = 2 * time.Second

	// followPollInterval is how often a following Tail checks for new output.
	// The log is a plain file appended to by a detached child, so there is no
	// notification to subscribe to.
	followPollInterval = 200 * time.Millisecond

	// maxLineBytes bounds what any tailer keeps of a single log line, so a
	// service emitting a megabyte-long line — or a progress bar that never
	// writes a newline — cannot exhaust memory in mabo-ctl or in a console
	// that streams the line. The log file itself is never shortened.
	maxLineBytes = 256 * 1024

	// tailReadBuffer is the read buffer behind every log line reader.
	tailReadBuffer = 64 * 1024

	// maxBacklogBytes bounds the history one tail request holds in memory and
	// sends. A line count alone does not: the web console allows 5000 lines,
	// and 5000 lines at maxLineBytes is over a gigabyte, re-read for every
	// request any console or shared link makes. Older lines past the budget
	// are replaced by one marker line saying how many were omitted.
	maxBacklogBytes = 8 << 20
)

// logTail returns the last n lines of svc's log, or "" when the log is absent
// or empty.
//
// It reads the whole file rather than seeking backwards. Logs are truncated on
// every start, so this is bounded by one run's output, and the alternative —
// a backwards block scan — is materially more code to get wrong for a path that
// only runs when a service has already failed.
func (s *Supervisor) logTail(svc string, n int) string {
	f, err := os.Open(s.st.LogPath(svc))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	ring := backlogRing{n: n}
	lr := newLineReader(f, 0)
	for {
		line, err := lr.next()
		if err != nil {
			break
		}
		ring.add(line)
	}
	// A dying service's last words often lack a newline; quote them anyway.
	if line, ok := lr.rest(); ok {
		ring.add(line)
	}
	return strings.Join(ring.result(), "\n")
}

// Tail streams svc's log to out.
//
// With follow false it sends the last n lines and returns. With follow true it
// sends the last n lines and then keeps sending new ones until ctx is done —
// including across a truncation, since every start empties the file and a
// tailer that did not notice would go silent for the rest of the session.
//
// Tail closes out when it returns, so a range over the channel terminates
// naturally. It starts no goroutines: the caller decides whether to run it in
// one.
func (s *Supervisor) Tail(ctx context.Context, svc string, n int, follow bool, out chan<- string) error {
	if _, ok := s.byName[svc]; !ok {
		close(out)
		return fmt.Errorf("%w: %s", ErrUnknownService, svc)
	}
	defer close(out)

	path := s.st.LogPath(svc)

	if !follow {
		for _, line := range strings.Split(s.logTail(svc, n), "\n") {
			if line == "" {
				continue
			}
			if !send(ctx, out, line) {
				return ctx.Err()
			}
		}
		return nil
	}

	f, err := os.Open(path)
	existed := err == nil
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("open %s: %w", path, err)
		}
		// The service may not have started yet. Wait for the file rather than
		// failing: in the TUI, selecting a stopped service and then starting it
		// must light the pane up without re-selecting.
		f, err = waitForFile(ctx, path)
		if err != nil {
			return err
		}
	}
	defer func() { _ = f.Close() }()

	// Snapshot the backlog through this same handle and follow from the exact
	// consumed offset. Seeking to EOF after sending the backlog loses concurrent
	// writes; doing so after waiting for a new file loses its startup output.
	var backlog []string
	var offset int64
	if existed {
		backlog, offset, err = followBacklog(f, n)
		if err != nil {
			return fmt.Errorf("backlog %s: %w", path, err)
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s: %w", path, err)
	}
	for _, line := range backlog {
		if !send(ctx, out, line) {
			return ctx.Err()
		}
	}

	// The live reader continues from the backlog's offset and keeps a partial
	// final line across polls instead of re-reading it: a line that never ends
	// costs one bounded prefix, not a fresh copy of itself every poll.
	lr := newLineReader(f, offset)
	tick := time.NewTicker(followPollInterval)
	defer tick.Stop()

	for {
		for {
			line, err := lr.next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			if !send(ctx, out, line) {
				return ctx.Err()
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}

		// Detect REPLACEMENT, not just truncation. A start rotates the old log
		// to <svc>.log.1 and creates a fresh file at the same path
		// (state.TruncateLog), and an open handle follows the INODE, not the
		// name: without this check the follower sits at the EOF of a dead file
		// while every line the new run prints goes into an inode it will never
		// see. Identity is compared against the PATH — a different inode there,
		// or the old shrink, means reopen and read from the top. Nothing can be
		// duplicated by rewinding to zero here: the replacement's contents were
		// never read under the old handle. A partial line of the old contents is
		// dropped with its reader.
		if fi, err := f.Stat(); err == nil {
			cur, serr := os.Stat(path)
			switch {
			case serr == nil && !sameFile(fi, cur):
				nf, oerr := os.Open(path)
				if oerr != nil {
					return fmt.Errorf("reopen %s after rotation: %w", path, oerr)
				}
				_ = f.Close()
				f = nf
				if _, serr2 := f.Seek(0, io.SeekStart); serr2 != nil {
					return fmt.Errorf("rewind %s after rotation: %w", path, serr2)
				}
				lr = newLineReader(f, 0)
			case serr == nil && cur.Size() < lr.consumed:
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return fmt.Errorf("rewind %s after truncation: %w", path, err)
				}
				lr = newLineReader(f, 0)
			}
		}
	}
}

// followBacklog reads a fixed snapshot of complete lines from f and reports the
// offset immediately after them. A partial final line stays for the live reader
// to deliver once its newline arrives, instead of being split across updates.
// An oversized line is truncated like every other tailed line, never an error:
// one long line must not make the whole log unreadable.
func followBacklog(f *os.File, n int) ([]string, int64, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	ring := backlogRing{n: n}
	lr := newLineReader(io.NewSectionReader(f, 0, info.Size()), 0)
	for {
		line, err := lr.next()
		if err == io.EOF {
			return ring.result(), lr.complete, nil
		}
		if err != nil {
			return nil, 0, err
		}
		ring.add(line)
	}
}

// backlogRing keeps the last n lines added, within maxBacklogBytes. The newest
// line is always kept: it is at most maxLineBytes, well inside the budget.
type backlogRing struct {
	n     int
	lines []string
	bytes int // total length of lines
	seen  int // lines added, kept or not
}

func (b *backlogRing) add(line string) {
	if b.n <= 0 {
		return
	}
	b.seen++
	b.lines = append(b.lines, line)
	b.bytes += len(line)
	for len(b.lines) > b.n || (b.bytes > maxBacklogBytes && len(b.lines) > 1) {
		b.bytes -= len(b.lines[0])
		b.lines[0] = "" // release it now, not when append next reallocates
		b.lines = b.lines[1:]
	}
}

// result returns the kept lines, led by a marker when the byte budget, not the
// line count, dropped any of the last n.
func (b *backlogRing) result() []string {
	if omitted := min(b.seen, b.n) - len(b.lines); omitted > 0 {
		marker := fmt.Sprintf("[mabo-ctl: %d earlier lines omitted; history is capped at %d MiB per request; the log file has all of them]",
			omitted, maxBacklogBytes>>20)
		return append([]string{marker}, b.lines...)
	}
	return b.lines
}

// lineReader splits a log into lines the way bufio.ScanLines does — "\n"
// separated, one trailing "\r" dropped — while retaining at most maxLineBytes
// of any line. Bytes past that are counted and discarded, and the delivered
// line says so. At the end of the available input next returns io.EOF and
// keeps the partial line, so a later call continues it once more is written.
type lineReader struct {
	r        *bufio.Reader
	kept     []byte // retained prefix of the line being read, ≤ maxLineBytes
	size     int    // bytes of that line read so far, its newline excluded
	cr       bool   // the last byte read of that line was '\r'
	consumed int64  // file offset just past everything read
	complete int64  // file offset just past the last complete line
}

func newLineReader(r io.Reader, offset int64) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, tailReadBuffer), consumed: offset, complete: offset}
}

// next returns the next complete line, or io.EOF (keeping any partial line)
// when no complete line is available yet, or the underlying read error.
func (lr *lineReader) next() (string, error) {
	for {
		frag, err := lr.r.ReadSlice('\n')
		lr.consumed += int64(len(frag))
		body := frag
		if err == nil {
			body = frag[:len(frag)-1]
		}
		if len(body) > 0 {
			lr.size += len(body)
			lr.cr = body[len(body)-1] == '\r'
			if room := maxLineBytes - len(lr.kept); room > 0 {
				lr.kept = append(lr.kept, body[:min(room, len(body))]...)
			}
		}
		switch err {
		case nil:
			lr.complete = lr.consumed
			return lr.take(), nil
		case bufio.ErrBufferFull:
			continue
		default:
			return "", err
		}
	}
}

// rest returns the partial final line, if any, consuming it.
func (lr *lineReader) rest() (string, bool) {
	if lr.consumed == lr.complete {
		return "", false
	}
	lr.complete = lr.consumed
	return lr.take(), true
}

// take returns the line read so far and resets for the next one.
func (lr *lineReader) take() string {
	n := lr.size
	if lr.cr {
		n--
	}
	var line string
	if n <= maxLineBytes {
		line = string(lr.kept[:n])
	} else {
		keep := lr.kept[:validPrefix(lr.kept)]
		line = fmt.Sprintf("%s … [mabo-ctl: line truncated to %d of %d bytes; the log file has all of it]",
			keep, len(keep), n)
	}
	lr.kept, lr.size, lr.cr = lr.kept[:0], 0, false
	return line
}

// validPrefix is the length of b without a trailing UTF-8 sequence that the
// truncation cut short. Invalid bytes elsewhere are left exactly as written.
func validPrefix(b []byte) int {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return len(b) - i
			}
			break
		}
	}
	return len(b)
}

// send delivers line unless ctx is cancelled first, reporting whether it went.
func send(ctx context.Context, out chan<- string, line string) bool {
	select {
	case out <- line:
		return true
	case <-ctx.Done():
		return false
	}
}

// waitForFile polls until path can be opened or ctx is done.
func waitForFile(ctx context.Context, path string) (*os.File, error) {
	tick := time.NewTicker(followPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
			if f, err := os.Open(path); err == nil {
				return f, nil
			}
		}
	}
}
