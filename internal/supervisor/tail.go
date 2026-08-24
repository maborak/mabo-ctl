package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
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

	// maxLineBytes bounds a single log line so a service emitting a
	// megabyte-long line cannot exhaust memory through the tailer.
	maxLineBytes = 256 * 1024
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

	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	return strings.Join(ring, "\n")
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

	// Emit the backlog first so a follower does not start from an empty pane.
	for _, line := range strings.Split(s.logTail(svc, n), "\n") {
		if line == "" {
			continue
		}
		if !send(ctx, out, line) {
			return ctx.Err()
		}
	}

	f, err := os.Open(path)
	if err != nil {
		// The service may not have started yet. Wait for the file rather than
		// failing: in the TUI, selecting a stopped service and then starting it
		// must light the pane up without re-selecting.
		f, err = waitForFile(ctx, path)
		if err != nil {
			return err
		}
	}
	defer func() { _ = f.Close() }()

	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek %s: %w", path, err)
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	tick := time.NewTicker(followPollInterval)
	defer tick.Stop()

	for {
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 && err == nil {
				offset += int64(len(line))
				if !send(ctx, out, strings.TrimRight(line, "\n")) {
					return ctx.Err()
				}
				continue
			}
			if err == io.EOF {
				// A partial final line stays buffered until its newline
				// arrives; rewinding keeps it from being emitted twice.
				if len(line) > 0 {
					if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
						return fmt.Errorf("rewind %s: %w", path, serr)
					}
					reader.Reset(f)
				}
				break
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}

		// Detect truncation: a start emptied the log, so the file we are
		// holding is shorter than where we are reading. Rewind to the top.
		if fi, err := f.Stat(); err == nil && fi.Size() < offset {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("rewind %s after truncation: %w", path, err)
			}
			offset = 0
			reader.Reset(f)
		}
	}
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
