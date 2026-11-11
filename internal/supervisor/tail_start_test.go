//go:build darwin || linux

package supervisor

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/service"
)

// observedTailContext signals when Tail first waits or sends, so writes occur
// after the initial snapshot without relying on scheduler timing or sleeps.
type observedTailContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedTailContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestTailFollowKeepsStartupAndConcurrentOutput(t *testing.T) {
	for _, tt := range []struct {
		name, initial, appended string
		want                    []string
	}{
		{"first start", "", "startup one\nstartup two\n", []string{"startup one", "startup two"}},
		{"append during backlog", "old one\nold two\n", "new output\n", []string{"old one", "old two", "new output"}},
		{"partial backlog line", "old\npartial", " completed\n", []string{"old", "partial completed"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sup, st := fixture(t, service.Instance{Name: "chatter"})
			if tt.initial != "" {
				f, err := st.TruncateLog("chatter")
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString(tt.initial)
				cerr := f.Close()
				if err != nil || cerr != nil {
					t.Fatal(errors.Join(err, cerr))
				}
			}
			base, cancel := context.WithCancel(context.Background())
			ctx := &observedTailContext{Context: base, observed: make(chan struct{})}
			lines := make(chan string)
			done := make(chan error, 1)
			go func() { done <- sup.Tail(ctx, "chatter", 10, true, lines) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Errorf("Tail shutdown: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("Tail did not stop after cancellation")
				}
			})
			select {
			case <-ctx.observed:
			case <-time.After(5 * time.Second):
				t.Fatal("Tail never waited or sent its backlog")
			}
			f, err := st.OpenLogAppend("chatter")
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(tt.appended)
			cerr := f.Close()
			if err != nil || cerr != nil {
				t.Fatal(errors.Join(err, cerr))
			}
			for _, want := range tt.want {
				select {
				case got, ok := <-lines:
					if !ok || got != want {
						t.Fatalf("Tail = %q (open %v), want %q", got, ok, want)
					}
				case <-time.After(3 * time.Second):
					t.Fatalf("Tail skipped %q", want)
				}
			}
		})
	}
}

func TestFollowBacklogCompleteLineOffsets(t *testing.T) {
	for _, tt := range []struct {
		name, text string
		n          int
		want       []string
		offset     int64
	}{
		{"limited history", "one\ntwo\nthree\npartial", 2, []string{"two", "three"}, 14},
		{"CRLF", "one\r\ntwo\r\npartial", 10, []string{"one", "two"}, 10},
		{"blank lines", "\nfirst\n\npartial", 10, []string{"", "first", ""}, 8},
		{"no history", "one\npartial", 0, nil, 4},
		{"partial only", "partial", 10, nil, 0},
		{"empty", "", 10, nil, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, st := fixture(t, service.Instance{Name: "chatter"})
			writer, err := st.TruncateLog("chatter")
			if err != nil {
				t.Fatal(err)
			}
			_, err = writer.WriteString(tt.text)
			cerr := writer.Close()
			if err != nil || cerr != nil {
				t.Fatal(errors.Join(err, cerr))
			}
			f, err := os.Open(st.LogPath("chatter"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			got, offset, err := followBacklog(f, tt.n)
			if err != nil || !reflect.DeepEqual(got, tt.want) || offset != tt.offset {
				t.Fatalf("followBacklog = %q, %d, %v; want %q, %d, nil", got, offset, err, tt.want, tt.offset)
			}
		})
	}
}
