package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/maborak/mabo-ctl/internal/service"
)

// The tty: lifecycle wiring around startOne. Real-pty coverage lives behind
// TestTTYBrokerRelaysThroughARealPty in the linux-gated build; everything here
// runs through scripted fakes so it executes on every supported platform.

func TestTTYStartOneWiringRunsTheBrokerAndRecordsItsPID(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-broker.sh")
	body := "#!/bin/sh\nprintf '{\"pid\":7777}\\n'\n"
	if werr := os.WriteFile(script, []byte(body), 0o755); werr != nil {
		t.Fatal(werr)
	}
	restore := ttyBrokerExecutable
	ttyBrokerExecutable = func() (string, error) { return script, nil }
	defer func() { ttyBrokerExecutable = restore }()

	sup, st := fixture(t, service.Instance{Name: "repl", TTY: true})
	evs := make(chan Event, 64)

	code, err := sup.startOne(context.Background(), sup.insts[0], evs)
	if err != nil {
		t.Fatalf("tty start failed: %v", err)
	}
	if code != PhaseRunning {
		t.Fatalf("phase = %v, want running (no probe declared)", code)
	}
	rec, _ := st.ReadPIDRecord("repl")
	if rec.PID != 7777 {
		t.Fatalf("pid record = %d, want the broker-reported 7777", rec.PID)
	}

	// Teardown without a real child behind pid 7777: stop must not wedge even
	// when its signal target is gone; any error is logged and ignored here so a
	// regression upstream still fails THIS test via phase/pid asserts above.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = sup.Stop(ctx, []string{"repl"}, evs)
}

// TestTTYSpawnRefusesWithoutABrokerExecutable keeps the failure honest: a
// broker that cannot be located is PhaseFailed naming why — never a silent
// fall-through to /dev/null stdin.
func TestTTYSpawnRefusesWithoutABrokerExecutable(t *testing.T) {
	restore := ttyBrokerExecutable
	ttyBrokerExecutable = func() (string, error) { return "", errors.New("no binary") }
	defer func() { ttyBrokerExecutable = restore }()

	sup, _ := fixture(t, service.Instance{Name: "repl", TTY: true})
	errs := make(chan Event, 16)
	code, err := sup.startOne(context.Background(), sup.insts[0], errs)
	if code != PhaseFailed || err == nil || !strings.Contains(err.Error(), "terminal broker") && !strings.Contains(err.Error(), "locate mabo-ctl") {
		t.Fatalf("phase=%v err=%v, want a named refusal", code, err)
	}
}

// TestTTYDarwinRefusalIsHonest pins the platform line where it can drift:
// darwin cannot allocate ptys from pure Go today and must SAY SO at start,
// not fail later inside an opaque ioctl error.
func TestTTYDarwinRefusalIsHonest(t *testing.T) {
	master, slavePath, err := openPty()
	if runtime.GOOS != "darwin" {
		if err != nil {
			t.Fatalf("%s openPty failed: %v", runtime.GOOS, err)
		}
		master.Close()
		return
	}
	if err == nil {
		master.Close()
		t.Fatalf("darwin unexpectedly allocated %s; update the refusal test", slavePath)
	}
	if !strings.Contains(err.Error(), "not yet supported on darwin") {
		t.Fatalf("refusal wording drifted: %v", err)
	}
}
