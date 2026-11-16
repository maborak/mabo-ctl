//go:build darwin || linux

package supervisor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/service"
)

const truncatedMark = "[mabo-ctl: line truncated to "

func TestLineReaderBoundsEachLine(t *testing.T) {
	t.Parallel()
	exact := strings.Repeat("a", maxLineBytes)
	for _, tt := range []struct {
		name, in string
		want     []string
	}{
		{"CRLF matches ScanLines", "one\r\ntwo\n\r\n", []string{"one", "two", ""}},
		{"exactly the bound with CRLF", exact + "\r\n", []string{exact}},
		{"one byte over", exact + "b\nnext\n", []string{exact + " … [mabo-ctl: line truncated to 262144 of 262145 bytes; the log file has all of it]", "next"}},
		{"cut rune is dropped, not split", strings.Repeat("a", maxLineBytes-1) + "é tail\n", []string{
			strings.Repeat("a", maxLineBytes-1) + " … [mabo-ctl: line truncated to 262143 of 262150 bytes; the log file has all of it]"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lr := newLineReader(strings.NewReader(tt.in), 0)
			var got []string
			for {
				line, err := lr.next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, line)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("lines differ:\n got %.120q\nwant %.120q", got, tt.want)
			}
			if lr.complete != int64(len(tt.in)) {
				t.Fatalf("complete = %d, want %d", lr.complete, len(tt.in))
			}
		})
	}
}

// TestLineReaderKeepsAnEndlessLineBounded is the memory guard: a line that
// keeps growing without a newline is continued across EOFs, never re-read,
// and never retained beyond maxLineBytes however long it gets.
func TestLineReaderKeepsAnEndlessLineBounded(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "svc.log")
	w, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	r, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	lr := newLineReader(r, 0)
	chunk := strings.Repeat("p", 64*1024)
	const chunks = 16 // 1 MiB: four times the bound
	for i := 0; i < chunks; i++ {
		if _, err := w.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
		if line, err := lr.next(); err != io.EOF {
			t.Fatalf("partial line delivered early: %.40q, %v", line, err)
		}
		if len(lr.kept) > maxLineBytes || cap(lr.kept) > 2*maxLineBytes {
			t.Fatalf("retained %d bytes (cap %d) of a partial line; bound is %d", len(lr.kept), cap(lr.kept), maxLineBytes)
		}
	}
	if _, err := w.WriteString("\nafter\n"); err != nil {
		t.Fatal(err)
	}
	line, err := lr.next()
	if err != nil || !strings.Contains(line, "truncated to 262144 of 1048576 bytes") || len(line) > maxLineBytes+200 {
		t.Fatalf("next = %d bytes ending %q, %v", len(line), line[max(0, len(line)-90):], err)
	}
	if line, err := lr.next(); err != nil || line != "after" {
		t.Fatalf("next = %q, %v; want after", line, err)
	}
	if want := int64(chunks*len(chunk) + len("\nafter\n")); lr.consumed != want || lr.complete != want {
		t.Fatalf("consumed=%d complete=%d, want %d", lr.consumed, lr.complete, want)
	}
}

// TestFollowBacklogSurvivesAnOversizedLine: one line over the bound used to
// fail the whole backlog (bufio.Scanner: token too long), leaving every
// console pane for that service dead in a reconnect loop.
func TestFollowBacklogSurvivesAnOversizedLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "svc.log")
	text := "before\n" + strings.Repeat("x", 300*1024) + "\nafter\npartial"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, offset, err := followBacklog(f, 10)
	if err != nil {
		t.Fatalf("followBacklog: %v", err)
	}
	if len(got) != 3 || got[0] != "before" || !strings.Contains(got[1], truncatedMark) || got[2] != "after" {
		t.Fatalf("backlog = %.80q", got)
	}
	if want := int64(len(text) - len("partial")); offset != want {
		t.Fatalf("offset = %d, want %d", offset, want)
	}
}

// TestBacklogStaysWithinItsByteBudget: a count of lines is not a memory bound
// when each line may be maxLineBytes long. The newest lines are kept, the
// omission is said, and the total stays inside maxBacklogBytes.
func TestBacklogStaysWithinItsByteBudget(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "svc.log")
	long := strings.Repeat("x", maxLineBytes)
	const total = 3 * maxBacklogBytes / maxLineBytes // three budgets' worth
	var b strings.Builder
	for i := 0; i < total; i++ {
		b.WriteString(long)
		b.WriteString("\n")
	}
	b.WriteString("newest\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, _, err := followBacklog(f, 5000)
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for _, l := range got[1:] {
		size += len(l)
	}
	kept := len(got) - 1
	want := fmt.Sprintf("[mabo-ctl: %d earlier lines omitted;", total+1-kept)
	if !strings.HasPrefix(got[0], want) || got[len(got)-1] != "newest" || size > maxBacklogBytes || kept < total/4 {
		t.Fatalf("backlog: %d lines, %d bytes, first %.90q, last %.20q; want marker %q", len(got), size, got[0], got[len(got)-1], want)
	}

	// A count limit alone drops lines silently, as asked: no marker.
	if got, _, err := followBacklog(f, 2); err != nil || len(got) != 2 || got[1] != "newest" || got[0] != long {
		t.Fatalf("followBacklog(n=2) = %d lines, %v", len(got), err)
	}
}

func TestLogTailKeepsLinesAfterAnOversizedOneAndTheLastWords(t *testing.T) {
	sup, st := fixture(t, service.Instance{Name: "svc"})
	text := strings.Repeat("x", 300*1024) + "\nafter\npanic: no newline"
	if err := os.WriteFile(st.LogPath("svc"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sup.logTail("svc", 2); got != "after\npanic: no newline" {
		t.Fatalf("logTail = %.120q", got)
	}
}

// TestTailFollowTruncatesAnEndlessLineAtTheSource drives the whole follower:
// the line every consumer (web, TUI, logs -f) receives is already bounded.
func TestTailFollowTruncatesAnEndlessLineAtTheSource(t *testing.T) {
	sup, st := fixture(t, service.Instance{Name: "svc"})
	logPath := st.LogPath("svc")
	if err := os.WriteFile(logPath, []byte("start\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 8)
	go func() { _ = sup.Tail(ctx, "svc", 10, true, lines) }()

	recv := func() string {
		t.Helper()
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("tail ended early")
			}
			return l
		case <-time.After(10 * time.Second):
			t.Fatal("no line within 10s")
		}
		return ""
	}
	if l := recv(); l != "start" {
		t.Fatalf("first line = %q", l)
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	chunk := strings.Repeat("p", 128*1024)
	for i := 0; i < 8; i++ { // 1 MiB, spread across several polls
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
		time.Sleep(followPollInterval / 2)
	}
	if _, err := f.WriteString("\nnext\n"); err != nil {
		t.Fatal(err)
	}
	if l := recv(); !strings.Contains(l, "truncated to 262144 of 1048576 bytes") || len(l) > maxLineBytes+200 {
		t.Fatalf("long line = %d bytes ending %q", len(l), l[max(0, len(l)-90):])
	}
	if l := recv(); l != "next" {
		t.Fatalf("line after the long one = %q", l)
	}
}
