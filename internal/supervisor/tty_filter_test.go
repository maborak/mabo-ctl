//go:build darwin || linux

package supervisor

import (
	"os"
	"strings"
	"testing"
)

// TestDetachFilterConsumesCtrlQ keeps the filter's contract pinned beside its
// only caller family: the detach byte stops the copy, is NOT forwarded, and
// everything typed before it still reaches the service.
func TestDetachFilterConsumesCtrlQ(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	go func() {
		_, _ = w.Write([]byte{'a', ttyDetachByte, 'b'})
		_ = w.Close()
	}()
	var sb strings.Builder
	err := detachFilterCopy(&sb, r)
	if err != nil && !strings.Contains(err.Error(), "file already closed") {
		// a benign race between the caller's deferred Close and the final EOF read
		t.Fatalf("filter err = %v", err)
	}
	if got := sb.String(); got != "a" {
		t.Fatalf("filter output = %q, want exactly the pre-detach prefix", got)
	}
}
