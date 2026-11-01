package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTerminalRunsAShellAndReplaysItsScrollback drives a real shell on a real
// pty: input reaches it, output reaches an attached client, a client that
// attaches later replays what it missed, and Close ends the session.
func TestTerminalRunsAShellAndReplaysItsScrollback(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	terms := NewTerminals(t.TempDir())
	t.Cleanup(terms.CloseAll)

	info, err := terms.Open(80, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	term, err := terms.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", info.ID, err)
	}
	_, out, detach := term.Attach()
	defer detach()

	if err := term.Resize(100, 30); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	// The arithmetic is done by the shell, so the marker only appears in the
	// output if the input really ran — the echoed command line never matches.
	if err := term.Write([]byte("echo mark-$((40+2))\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var got bytes.Buffer
	deadline := time.After(10 * time.Second)
	for !strings.Contains(got.String(), "mark-42") {
		select {
		case chunk, ok := <-out:
			if !ok {
				t.Fatalf("output closed before the marker; got %q", got.String())
			}
			got.Write(chunk)
		case <-deadline:
			t.Fatalf("no marker after 10s; got %q", got.String())
		}
	}

	backlog, _, detachLate := term.Attach()
	detachLate()
	if !bytes.Contains(backlog, []byte("mark-42")) {
		t.Errorf("a late client's scrollback lacks the marker: %q", backlog)
	}
	if n := len(terms.List()); n != 1 {
		t.Errorf("List has %d terminals, want 1", n)
	}

	if err := terms.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-term.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the shell outlived Close")
	}
	waitFor(t, func() bool { return len(terms.List()) == 0 })
	if _, err := terms.Get(info.ID); !errors.Is(err, ErrNoTerminal) {
		t.Errorf("Get after Close = %v, want ErrNoTerminal", err)
	}
}

// TestTerminalEndsWhenItsShellExits checks that an `exit` typed into the shell
// ends the terminal and closes every attached client.
func TestTerminalEndsWhenItsShellExits(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	terms := NewTerminals(t.TempDir())
	t.Cleanup(terms.CloseAll)

	info, err := terms.Open(80, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	term, _ := terms.Get(info.ID)
	_, out, detach := term.Attach()
	defer detach()
	if err := term.Write([]byte("exit\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-out:
			if !ok {
				waitFor(t, func() bool { return len(terms.List()) == 0 })
				return
			}
		case <-deadline:
			t.Fatal("the terminal did not end after exit")
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTerminalEndsWhenItsShellExitsEvenIfThePtyStaysOpen pins that a terminal
// ends with its shell. A process that left the shell's session keeps the pty
// open, so a terminal that waited for the pty's end of file would never end —
// and Close, and serve's shutdown with it, would hang behind it.
func TestTerminalEndsWhenItsShellExitsEvenIfThePtyStaysOpen(t *testing.T) {
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("perl is needed to detach a process into its own session")
	}
	t.Setenv("SHELL", "/bin/sh")
	terms := NewTerminals(t.TempDir())
	t.Cleanup(terms.CloseAll)

	info, err := terms.Open(80, 24)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	term, _ := terms.Get(info.ID)
	// The detached sleeper inherits the pty as its stdio and outlives the
	// shell's session; its pid is printed so the test can clean it up.
	cmd := perl + ` -e 'use POSIX qw(setsid); fork and exit; setsid(); print "orphan=$$\n"; sleep 30' ; sleep 0.3; exit` + "\n"
	if err := term.Write([]byte(cmd)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case <-term.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the terminal did not end within 5s of its shell exiting")
	}
	backlog, _, detach := term.Attach()
	detach()
	// The last occurrence: the first is the echoed command line itself.
	if i := bytes.LastIndex(backlog, []byte("orphan=")); i >= 0 {
		var pid int
		if _, err := fmt.Sscanf(string(backlog[i:]), "orphan=%d", &pid); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// TestTerminalsAreCapped checks that MaxTerminals is enforced.
func TestTerminalsAreCapped(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	terms := NewTerminals(t.TempDir())
	t.Cleanup(terms.CloseAll)
	for i := 0; i < MaxTerminals; i++ {
		if _, err := terms.Open(80, 24); err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
	}
	if _, err := terms.Open(80, 24); !errors.Is(err, ErrTooManyTerminals) {
		t.Fatalf("Open past the cap = %v, want ErrTooManyTerminals", err)
	}
}
