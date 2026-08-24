package state

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestPIDRecordRoundTripCarriesTheSpawnTime(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	started := time.Date(2026, 8, 16, 9, 12, 0, 0, time.UTC)

	if err := d.WritePIDAt("backend", 4242, started); err != nil {
		t.Fatalf("WritePIDAt: %v", err)
	}
	rec, err := d.ReadPIDRecord("backend")
	if err != nil {
		t.Fatalf("ReadPIDRecord: %v", err)
	}
	if rec.PID != 4242 {
		t.Errorf("PID = %d, want 4242", rec.PID)
	}
	if !rec.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %s, want %s", rec.StartedAt, started)
	}
	// The narrow reader still answers the same question.
	if pid, err := d.ReadPID("backend"); err != nil || pid != 4242 {
		t.Errorf("ReadPID = (%d, %v), want (4242, nil)", pid, err)
	}
	if got := mode(t, d.PIDPath("backend")); got != 0o600 {
		t.Errorf("pid file mode = %04o, want 0600", got)
	}
}

func TestWritePIDStampsNow(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	before := time.Now()
	if err := d.WritePID("backend", 4242); err != nil {
		t.Fatalf("WritePID: %v", err)
	}
	after := time.Now()

	rec, err := d.ReadPIDRecord("backend")
	if err != nil {
		t.Fatalf("ReadPIDRecord: %v", err)
	}
	if rec.StartedAt.Before(before) || rec.StartedAt.After(after) {
		t.Errorf("StartedAt = %s, want it between %s and %s", rec.StartedAt, before, after)
	}
}

func TestPIDFileIsAJSONRecord(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := d.WritePIDAt("backend", 7, time.Unix(1700000000, 0).UTC()); err != nil {
		t.Fatalf("WritePIDAt: %v", err)
	}
	b, err := os.ReadFile(d.PIDPath("backend"))
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("pid file is not JSON: %v (%q)", err, b)
	}
	for _, k := range []string{"pid", "started_at"} {
		if _, present := raw[k]; !present {
			t.Errorf("pid file has no %q key: %q", k, b)
		}
	}
}

// A user who upgrades mabo-ctl mid-session has running services whose pid files
// were written by the old binary as a bare integer. Refusing to parse them
// would make every one of those services unmanageable — start would refuse and
// stop would have nothing to signal — so the legacy format is still read.
func TestReadPIDAcceptsALegacyBareIntegerFile(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := os.WriteFile(d.PIDPath("backend"), []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("seed legacy pid file: %v", err)
	}

	pid, err := d.ReadPID("backend")
	if err != nil {
		t.Fatalf("ReadPID of a legacy file: %v", err)
	}
	if pid != 4242 {
		t.Errorf("ReadPID of a legacy file = %d, want 4242", pid)
	}

	rec, err := d.ReadPIDRecord("backend")
	if err != nil {
		t.Fatalf("ReadPIDRecord of a legacy file: %v", err)
	}
	if rec.PID != 4242 {
		t.Errorf("PID = %d, want 4242", rec.PID)
	}
	// No spawn time was recorded, and none may be invented: zero means unknown.
	if !rec.StartedAt.IsZero() {
		t.Errorf("StartedAt = %s for a legacy file, want the zero time", rec.StartedAt)
	}
}

func TestReadPIDRecordMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		contents string
	}{
		{"truncated object", `{"pid":42,`},
		{"record with a zero pid", `{"pid":0,"started_at":"2026-08-16T09:12:00Z"}`},
		{"record with a negative pid", `{"pid":-9}`},
		{"record with a non-numeric pid", `{"pid":"forty-two"}`},
		{"record with an unparseable time", `{"pid":42,"started_at":"yesterday"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := newDir(t)
			if err := os.WriteFile(d.PIDPath("backend"), []byte(c.contents), 0o600); err != nil {
				t.Fatalf("seed pid file: %v", err)
			}
			rec, err := d.ReadPIDRecord("backend")
			if err == nil {
				t.Fatalf("ReadPIDRecord(%q) = (%+v, nil), want ErrMalformedPID", c.contents, rec)
			}
			if !errors.Is(err, ErrMalformedPID) {
				t.Errorf("ReadPIDRecord(%q) error = %v, want it to wrap ErrMalformedPID", c.contents, err)
			}
			if rec.PID != 0 {
				t.Errorf("ReadPIDRecord(%q) PID = %d, want 0 alongside the error", c.contents, rec.PID)
			}
		})
	}
}

// A record from a mabo-ctl newer than this one may carry fields this build has
// never heard of. Reading it must not fail: the pid is what matters, and a
// downgrade that cannot name its own running processes is the same wedge the
// legacy-format tolerance exists to prevent.
func TestReadPIDRecordIgnoresUnknownFields(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	body := `{"pid":4242,"started_at":"2026-08-16T09:12:00Z","boot_id":"abc","cmdline":"uvicorn"}`
	if err := os.WriteFile(d.PIDPath("backend"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed pid file: %v", err)
	}
	rec, err := d.ReadPIDRecord("backend")
	if err != nil {
		t.Fatalf("ReadPIDRecord: %v", err)
	}
	if rec.PID != 4242 {
		t.Errorf("PID = %d, want 4242", rec.PID)
	}
}

func TestReadPIDRecordAbsentAndInvalidName(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	rec, err := d.ReadPIDRecord("never-started")
	if err != nil || rec != (PIDRecord{}) {
		t.Fatalf("ReadPIDRecord of absent file = (%+v, %v), want (zero, nil)", rec, err)
	}
	if _, err := d.ReadPIDRecord("../evil"); !errors.Is(err, ErrInvalidService) {
		t.Errorf("ReadPIDRecord(\"../evil\") error = %v, want ErrInvalidService", err)
	}
	if err := d.WritePIDAt("../evil", 5, time.Now()); !errors.Is(err, ErrInvalidService) {
		t.Errorf("WritePIDAt(\"../evil\") error = %v, want ErrInvalidService", err)
	}
}

func TestWritePIDAtRejectsNonPositive(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	for _, pid := range []int{0, -1} {
		if err := d.WritePIDAt("backend", pid, time.Now()); err == nil {
			t.Errorf("WritePIDAt(backend, %d) = nil, want an error", pid)
		}
	}
	if _, err := os.Stat(d.PIDPath("backend")); err == nil {
		t.Errorf("a rejected WritePIDAt created %s", d.PIDPath("backend"))
	}
}
