//go:build darwin || linux

package supervisor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/service"
)

// TestTailFollowsTheLogAcrossARotation: a start ROTATES the previous log to
// <svc>.log.1 and creates a fresh file at the same path, and an attached
// follower must follow the NAME, not the inode it happens to hold — otherwise
// every web pane, TUI pane and `logs -f` goes silent after any restart, which
// is what shipped with the log-rotation feature until this test existed.
func TestTailFollowsTheLogAcrossARotation(t *testing.T) {
	sup, st := fixture(t, service.Instance{Name: "chatter"})

	logPath := st.LogPath("chatter")
	if err := os.WriteFile(logPath, []byte("OLD-RUN-1\nOLD-RUN-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 64)
	go func() { _ = sup.Tail(ctx, "chatter", 50, true, lines) }()

	await := func(substr string, what string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("%s: tail ended early", what)
				}
				if strings.Contains(l, substr) {
					return
				}
			case <-deadline:
				t.Fatalf("%s: never saw %q", what, substr)
			}
		}
	}

	await("OLD-RUN-1", "backlog")

	// Exactly what startOne does between stopping the old child and probing
	// the new one: rotate + create, write THROUGH THE NEW HANDLE.
	f, err := st.TruncateLog("chatter")
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.WriteString("NEW-RUN-A\nNEW-RUN-B\n"); werr != nil {
		t.Fatal(werr)
	}
	f.Close()

	await("NEW-RUN-A", "post-rotation output")
	await("NEW-RUN-B", "rest of the new run")
	cancel()
}
