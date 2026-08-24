package state

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// seedRunEnv writes raw run.env contents, bypassing WriteRunEnv, so tests can
// present the parser with exactly the bytes they mean.
func seedRunEnv(t *testing.T, d *Dir, contents string) {
	t.Helper()
	if err := os.WriteFile(d.RunEnvPath(), []byte(contents), 0o600); err != nil {
		t.Fatalf("seed run.env: %v", err)
	}
}

func readRunEnvRaw(t *testing.T, d *Dir) string {
	t.Helper()
	b, err := os.ReadFile(d.RunEnvPath())
	if err != nil {
		t.Fatalf("read run.env: %v", err)
	}
	return string(b)
}

func TestReadRunEnvMissingFileIsEmptyNotAnError(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	re, err := d.ReadRunEnv()
	if err != nil {
		t.Fatalf("ReadRunEnv with no file: %v, want nil (nothing resolved yet)", err)
	}
	if re == nil {
		t.Fatal("ReadRunEnv returned a nil RunEnv")
	}
	if len(re.Ports) != 0 {
		t.Errorf("Ports = %v, want empty", re.Ports)
	}
	if re.Malformed() != 0 {
		t.Errorf("Malformed = %d, want 0", re.Malformed())
	}
	if len(re.Unknown()) != 0 {
		t.Errorf("Unknown = %v, want empty", re.Unknown())
	}
}

func TestRunEnvRoundTrip(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	want := map[string]int{"website": 7100, "frontend": 7101, "backend": 7102}
	if err := d.WriteRunEnv(&RunEnv{Ports: want}); err != nil {
		t.Fatalf("WriteRunEnv: %v", err)
	}
	re, err := d.ReadRunEnv()
	if err != nil {
		t.Fatalf("ReadRunEnv: %v", err)
	}
	if !reflect.DeepEqual(re.Ports, want) {
		t.Errorf("Ports = %v, want %v", re.Ports, want)
	}
	if got := mode(t, d.RunEnvPath()); got != 0o600 {
		t.Errorf("run.env mode = %04o, want 0600 (it can carry resolved infrastructure detail)", got)
	}
	raw := readRunEnvRaw(t, d)
	for _, line := range []string{"PORT_WEBSITE=7100", "PORT_FRONTEND=7101", "PORT_BACKEND=7102"} {
		if !strings.Contains(raw, line) {
			t.Errorf("run.env %q missing line %q", raw, line)
		}
	}
	// A stable file means a readable diff: port keys are sorted.
	var keys []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "PORT_") {
			keys = append(keys, line)
		}
	}
	if !sort.StringsAreSorted(keys) {
		t.Errorf("port lines %v are not sorted", keys)
	}
}

func TestRunEnvZeroPortRoundTrips(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	// 0 means "portless service"; it must survive rather than be dropped.
	if err := d.WriteRunEnv(&RunEnv{Ports: map[string]int{"worker": 0}}); err != nil {
		t.Fatalf("WriteRunEnv: %v", err)
	}
	re, err := d.ReadRunEnv()
	if err != nil {
		t.Fatalf("ReadRunEnv: %v", err)
	}
	if p, ok := re.Port("worker"); !ok || p != 0 {
		t.Errorf("Port(worker) = (%d, %v), want (0, true)", p, ok)
	}
}

func TestRunEnvPreservesUnknownKeysOnRewrite(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	seedRunEnv(t, d, strings.Join([]string{
		"# written by a future mabo-ctl",
		"PORT_BACKEND=7102",
		"STATE_VERSION=3",
		"RESOLVED_AT=2026-08-15T10:00:00Z",
		"",
	}, "\n"))

	re, err := d.ReadRunEnv()
	if err != nil {
		t.Fatalf("ReadRunEnv: %v", err)
	}
	unknown := re.Unknown()
	if unknown["STATE_VERSION"] != "3" || unknown["RESOLVED_AT"] != "2026-08-15T10:00:00Z" {
		t.Fatalf("Unknown = %v, want STATE_VERSION and RESOLVED_AT preserved", unknown)
	}
	if re.Ports["backend"] != 7102 {
		t.Fatalf("Ports = %v, want backend=7102", re.Ports)
	}

	re.SetPort("backend", 7999)
	re.SetPort("website", 7100)
	if err := d.WriteRunEnv(re); err != nil {
		t.Fatalf("WriteRunEnv: %v", err)
	}

	raw := readRunEnvRaw(t, d)
	for _, line := range []string{"STATE_VERSION=3", "RESOLVED_AT=2026-08-15T10:00:00Z", "PORT_BACKEND=7999", "PORT_WEBSITE=7100"} {
		if !strings.Contains(raw, line) {
			t.Errorf("rewritten run.env %q lost %q", raw, line)
		}
	}
	if strings.Contains(raw, "PORT_BACKEND=7102") {
		t.Errorf("rewritten run.env %q kept the stale port", raw)
	}
}

func TestWriteRunEnvPreservesUnknownKeysFromDisk(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	seedRunEnv(t, d, "STATE_VERSION=3\nPORT_BACKEND=7102\n")

	// A caller that never read the file must not drop another writer's key.
	fresh := &RunEnv{Ports: map[string]int{"backend": 7102}}
	if err := d.WriteRunEnv(fresh); err != nil {
		t.Fatalf("WriteRunEnv: %v", err)
	}
	if raw := readRunEnvRaw(t, d); !strings.Contains(raw, "STATE_VERSION=3") {
		t.Errorf("run.env %q dropped an unknown key written by another version", raw)
	}
}

func TestRunEnvJunkLinesAreSkippedAndCounted(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	seedRunEnv(t, d, strings.Join([]string{
		"# a comment is not junk",
		"",
		"   ",
		"PORT_BACKEND=7102",
		"this line has no equals sign",
		"=novalue",
		"PORT_WEBSITE=not-a-number",
		"PORT_FRONTEND=70000",
		"PORT_WORKER=-1",
		`PORT_BROWSER="7103"`,
		"PORT_=7104",
		"KEEP_ME=yes",
		"",
	}, "\n"))

	re, err := d.ReadRunEnv()
	if err != nil {
		t.Fatalf("ReadRunEnv: %v", err)
	}
	want := map[string]int{"backend": 7102, "browser": 7103}
	if !reflect.DeepEqual(re.Ports, want) {
		t.Errorf("Ports = %v, want %v", re.Ports, want)
	}
	// no-equals, empty key, not-a-number, out of range, negative = 5.
	if re.Malformed() != 5 {
		t.Errorf("Malformed = %d, want 5", re.Malformed())
	}
	unknown := re.Unknown()
	if unknown["KEEP_ME"] != "yes" {
		t.Errorf("Unknown = %v, want KEEP_ME preserved", unknown)
	}
	if _, ok := unknown["PORT_"]; !ok {
		t.Errorf("Unknown = %v, want the bare PORT_ key kept as an unknown key", unknown)
	}

	// Junk must never become fatal: a rewrite still succeeds and drops it.
	if err := d.WriteRunEnv(re); err != nil {
		t.Fatalf("WriteRunEnv after junk: %v", err)
	}
	raw := readRunEnvRaw(t, d)
	if strings.Contains(raw, "not-a-number") {
		t.Errorf("rewritten run.env %q kept a junk port value", raw)
	}
	if !strings.Contains(raw, "KEEP_ME=yes") {
		t.Errorf("rewritten run.env %q lost an unknown key", raw)
	}
}

func TestRunEnvLastDuplicateWins(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	seedRunEnv(t, d, "PORT_BACKEND=7102\nPORT_BACKEND=7999\nFOO=a\nFOO=b\n")
	re, err := d.ReadRunEnv()
	if err != nil {
		t.Fatalf("ReadRunEnv: %v", err)
	}
	if re.Ports["backend"] != 7999 {
		t.Errorf("Ports[backend] = %d, want 7999", re.Ports["backend"])
	}
	if got := re.Unknown()["FOO"]; got != "b" {
		t.Errorf("Unknown[FOO] = %q, want %q", got, "b")
	}
}

func TestRunEnvCRLFAndQuotedValues(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	seedRunEnv(t, d, "PORT_BACKEND=7102\r\nPORT_WEBSITE='7100'\r\n")
	re, err := d.ReadRunEnv()
	if err != nil {
		t.Fatalf("ReadRunEnv: %v", err)
	}
	want := map[string]int{"backend": 7102, "website": 7100}
	if !reflect.DeepEqual(re.Ports, want) {
		t.Errorf("Ports = %v, want %v", re.Ports, want)
	}
}

func TestRunEnvHyphenatedServiceName(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := d.WriteRunEnv(&RunEnv{Ports: map[string]int{"browser-service": 7103}}); err != nil {
		t.Fatalf("WriteRunEnv: %v", err)
	}
	if raw := readRunEnvRaw(t, d); !strings.Contains(raw, "PORT_BROWSER_SERVICE=7103") {
		t.Errorf("run.env %q, want a shell-safe PORT_BROWSER_SERVICE key", raw)
	}
	re, err := d.ReadRunEnv()
	if err != nil {
		t.Fatalf("ReadRunEnv: %v", err)
	}
	if p, ok := re.Port("browser-service"); !ok || p != 7103 {
		t.Errorf("Port(browser-service) = (%d, %v), want (7103, true)", p, ok)
	}
	if p, ok := re.Port("BROWSER_SERVICE"); !ok || p != 7103 {
		t.Errorf("Port(BROWSER_SERVICE) = (%d, %v), want (7103, true)", p, ok)
	}
	if _, ok := re.Port("nope"); ok {
		t.Error("Port(nope) reported a hit")
	}
}

func TestWriteRunEnvRejectsCollidingKeys(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	err := d.WriteRunEnv(&RunEnv{Ports: map[string]int{"a-b": 1, "a_b": 2}})
	if err == nil {
		t.Fatal("WriteRunEnv with two names sharing one key = nil, want an error naming both")
	}
	for _, want := range []string{"a-b", "a_b", "PORT_A_B"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if _, statErr := os.Stat(d.RunEnvPath()); statErr == nil {
		t.Error("a rejected WriteRunEnv wrote run.env anyway")
	}
}

func TestWriteRunEnvRejectsUnsafeServiceName(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := d.WriteRunEnv(&RunEnv{Ports: map[string]int{"../evil": 1}}); err == nil {
		t.Fatal("WriteRunEnv with an unsafe name = nil, want an error")
	}
}

func TestWriteRunEnvNil(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	if err := d.WriteRunEnv(nil); err == nil {
		t.Fatal("WriteRunEnv(nil) = nil, want an error")
	}
}

func TestRunEnvSetPortOnZeroValue(t *testing.T) {
	t.Parallel()
	var re RunEnv
	re.SetPort("backend", 7102)
	if p, ok := re.Port("backend"); !ok || p != 7102 {
		t.Errorf("Port(backend) = (%d, %v), want (7102, true)", p, ok)
	}
}

func TestRunEnvUnknownReturnsACopy(t *testing.T) {
	t.Parallel()
	d := newDir(t)
	seedRunEnv(t, d, "KEEP_ME=yes\n")
	re, err := d.ReadRunEnv()
	if err != nil {
		t.Fatalf("ReadRunEnv: %v", err)
	}
	re.Unknown()["KEEP_ME"] = "tampered"
	if err := d.WriteRunEnv(re); err != nil {
		t.Fatalf("WriteRunEnv: %v", err)
	}
	if raw := readRunEnvRaw(t, d); !strings.Contains(raw, "KEEP_ME=yes") {
		t.Errorf("run.env %q, want the caller's mutation of the Unknown copy to be ignored", raw)
	}
}

func TestNilRunEnvPortIsSafe(t *testing.T) {
	t.Parallel()
	var re *RunEnv
	if p, ok := re.Port("backend"); ok || p != 0 {
		t.Errorf("(*RunEnv)(nil).Port = (%d, %v), want (0, false)", p, ok)
	}
}

// TestConcurrentWriteRunEnvDoesNotLoseAForeignKey covers the read-modify-write
// that two mabo-ctl invocations perform on the same file.
//
// WriteRunEnv does not simply overwrite: it RE-READS run.env for the keys it
// does not own and writes them back, so that a tool or a human who added a
// variable does not lose it. Read and write are two syscalls, and unlocked, two
// writers both read the same snapshot and the second rewrite discards whatever
// the first one had just added.
//
// (Port keys are deliberately NOT part of this: WriteRunEnv is documented to
// take the FULL service list and to drop port keys absent from it, so a
// "missing" port from a partial write is the contract, not a race.)
func TestConcurrentWriteRunEnvDoesNotLoseAForeignKey(t *testing.T) {
	t.Parallel()
	d := newDir(t)

	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// The read-modify-write a second mabo-ctl performs: take what is on
			// disk, add this writer's own key, put it all back.
			re, err := d.ReadRunEnv()
			if err != nil {
				t.Errorf("ReadRunEnv: %v", err)
				return
			}
			re.Ports = map[string]int{"backend": 7102}
			re.unknown = append(re.unknown, rawEntry{
				key:   fmt.Sprintf("FOREIGN_%d", i),
				value: strconv.Itoa(i),
			})
			if err := d.WriteRunEnv(re); err != nil {
				t.Errorf("WriteRunEnv: %v", err)
			}
		}(i)
	}
	wg.Wait()

	raw := readRunEnvRaw(t, d)
	var lost []string
	for i := 0; i < writers; i++ {
		if !strings.Contains(raw, fmt.Sprintf("FOREIGN_%d=%d", i, i)) {
			lost = append(lost, fmt.Sprintf("FOREIGN_%d", i))
		}
	}
	if len(lost) > 0 {
		t.Errorf("concurrent writes lost %d of %d foreign keys (%s):\n%s",
			len(lost), writers, strings.Join(lost, ", "), raw)
	}
}
