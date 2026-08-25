// Package state owns `.dev/`, mabo-ctl's on-disk state directory: per-service log
// files, per-service pid records, per-service exit records and the persisted
// resolved-port cache (`run.env`). It is the only package in mabo-ctl that writes
// under `.dev/`.
//
// Everything here is treated as secrets-adjacent. A supervised service prints
// whatever it likes on stdout and that stdout lands in `.dev/logs/<svc>.log` —
// and, truncated, in `.dev/exits/<svc>.json` — so directories are created 0700
// and files 0600; nothing under `.dev/` is ever created group- or
// world-readable.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// dirPerm is the mode of every directory mabo-ctl creates under the repo root.
	dirPerm fs.FileMode = 0o700
	// filePerm is the mode of every file mabo-ctl creates under `.dev/`.
	filePerm fs.FileMode = 0o600

	stateDirName = ".dev"
	logsDirName  = "logs"
	pidsDirName  = "pids"
	exitsDirName = "exits"
	runEnvName   = "run.env"
)

// ErrInvalidService is returned when a service name cannot safely compose a
// path under `.dev/`. A name containing a path separator or `..` is a path
// traversal that would write outside the state directory, so state rejects it
// even though config is expected to have caught it at load time.
var ErrInvalidService = errors.New("invalid service name")

// ErrMalformedPID is returned by Dir.ReadPID when a pid file exists but does
// not hold a single positive integer. An absent pid file is not an error — it
// means "not running" — but a corrupt one is, because acting on a garbage pid
// means signalling an unrelated process.
var ErrMalformedPID = errors.New("malformed pid file")

// Dir is the `.dev/` state directory for one repository. Root is the repository
// root; the state itself lives at Root/.dev. The zero value is not usable —
// construct a Dir with New.
type Dir struct {
	// Root is the absolute path of the repository root that owns this state.
	Root string
}

// New creates the state directory tree for the repository rooted at root:
// `.dev/`, `.dev/logs/`, `.dev/pids/` and `.dev/exits/`, each mode 0700.
// Directories that already exist are chmodded back to 0700, because a log file
// that inherits a world-readable directory from an earlier version is a
// disclosure of whatever the supervised service printed — and an exit record
// carries a slice of that same output. New is idempotent.
//
// It returns an error if root is empty, cannot be made absolute, or any part of
// the tree cannot be created.
func New(root string) (*Dir, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("state: New: empty repository root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("state: resolve root %q: %w", root, err)
	}
	d := &Dir{Root: abs}
	for _, p := range []string{d.Path(), d.LogsDir(), d.PIDsDir(), d.ExitsDir()} {
		if err := os.MkdirAll(p, dirPerm); err != nil {
			return nil, fmt.Errorf("state: create %s: %w", p, err)
		}
		if err := os.Chmod(p, dirPerm); err != nil {
			return nil, fmt.Errorf("state: chmod %s to %o: %w", p, dirPerm, err)
		}
	}
	return d, nil
}

// Path returns the absolute path of the state directory itself (Root/.dev).
func (d *Dir) Path() string { return filepath.Join(d.Root, stateDirName) }

// LogsDir returns the absolute path of the log directory (Root/.dev/logs).
func (d *Dir) LogsDir() string { return filepath.Join(d.Path(), logsDirName) }

// PIDsDir returns the absolute path of the pid directory (Root/.dev/pids).
func (d *Dir) PIDsDir() string { return filepath.Join(d.Path(), pidsDirName) }

// RunEnvPath returns the absolute path of the resolved-port cache
// (Root/.dev/run.env).
func (d *Dir) RunEnvPath() string { return filepath.Join(d.Path(), runEnvName) }

// RunEnvLockPath returns the advisory lock guarding run.env's read-modify-write:
// Root/.dev/run.env.lock.
//
// It is a separate file from run.env because run.env is replaced by an atomic
// rename, and a lock held on the replaced inode guards nothing. Like every other
// file under `.dev/`, it is disposable — `mabo-ctl reset` removes the whole tree.
func (d *Dir) RunEnvLockPath() string { return filepath.Join(d.Path(), runEnvName+".lock") }

// LogPath returns the log file path for svc: Root/.dev/logs/<svc>.log. It does
// not validate svc; callers that create or read the file go through methods
// that do. Passing an unvalidated name here can escape `.dev/`.
func (d *Dir) LogPath(svc string) string {
	return filepath.Join(d.LogsDir(), svc+".log")
}

// PIDPath returns the pid file path for svc: Root/.dev/pids/<svc>.pid. It does
// not validate svc; callers that create or read the file go through methods
// that do. Passing an unvalidated name here can escape `.dev/`.
func (d *Dir) PIDPath(svc string) string {
	return filepath.Join(d.PIDsDir(), svc+".pid")
}

// PIDRecord is what `.dev/pids/<svc>.pid` holds: the process mabo-ctl spawned for
// a service and the instant it spawned it.
//
// StartedAt is on disk rather than in memory because uptime has to outlive the
// process that knows it. Every one-shot invocation — `mabo-ctl status`, and the
// web console behind it — is a different process from the one that did the
// spawning, so "this has been up four hours" is unanswerable unless the spawn
// time was written down.
type PIDRecord struct {
	// PID is the process id mabo-ctl spawned. In any record ReadPIDRecord returns
	// without an error it is positive.
	PID int `json:"pid"`
	// StartedAt is when mabo-ctl spawned PID. It is the zero time for a legacy
	// bare-integer pid file, which carried no spawn time — callers must treat
	// IsZero as "unknown", never as the epoch.
	StartedAt time.Time `json:"started_at"`
}

// WritePID records pid as the supervised process for svc, stamping the spawn
// time as now. It is WritePIDAt for the common case, where the caller is
// writing the record immediately after the spawn it describes.
func (d *Dir) WritePID(svc string, pid int) error {
	return d.WritePIDAt(svc, pid, time.Now())
}

// WritePIDAt records pid as the supervised process for svc, spawned at
// startedAt. The file is written atomically (temp file plus rename) with mode
// 0600 so a concurrent ReadPID never observes a half-written record.
//
// It returns an error wrapping ErrInvalidService when svc is not a safe name,
// an error when pid is not positive, or any filesystem error.
func (d *Dir) WritePIDAt(svc string, pid int, startedAt time.Time) error {
	if err := validService(svc); err != nil {
		return err
	}
	if pid <= 0 {
		return fmt.Errorf("state: write pid for %q: pid %d is not positive", svc, pid)
	}
	b, err := json.Marshal(PIDRecord{PID: pid, StartedAt: startedAt})
	if err != nil {
		return fmt.Errorf("state: encode pid record for %q: %w", svc, err)
	}
	p := d.PIDPath(svc)
	if err := writeFileAtomic(p, append(b, '\n'), filePerm); err != nil {
		return fmt.Errorf("state: write pid file %s: %w", p, err)
	}
	return nil
}

// ReadPID returns the pid recorded for svc, discarding the spawn time. It is
// ReadPIDRecord for the callers that only need to know what to signal.
//
// An absent pid file returns (0, nil): "no pid file" means "not running", which
// is a normal state and not an error. A corrupt one returns an error wrapping
// ErrMalformedPID, and 0 alongside it.
func (d *Dir) ReadPID(svc string) (int, error) {
	rec, err := d.ReadPIDRecord(svc)
	return rec.PID, err
}

// ReadPIDRecord returns the pid record for svc.
//
// An absent pid file returns (zero, nil): "no pid file" means "not running",
// which is a normal state and not an error. A pid file that exists but holds
// neither a decodable record nor a single positive integer returns an error
// wrapping ErrMalformedPID — a corrupt pid must never be silently treated as 0
// or acted upon, because signalling a garbage pid hits an unrelated process.
//
// Two on-disk formats are accepted. The current one is a JSON PIDRecord. The
// legacy one is a bare decimal integer, which is what mabo-ctl wrote before the
// record carried a spawn time; it is still read because upgrading the binary
// must not make an already-running stack unmanageable — the running services
// belong to the old pid files, and refusing to parse them would leave every
// start refusing and every stop unable to name what to signal. A legacy file
// yields a zero StartedAt, which is "unknown", not "1 January year 1".
func (d *Dir) ReadPIDRecord(svc string) (PIDRecord, error) {
	if err := validService(svc); err != nil {
		return PIDRecord{}, err
	}
	p := d.PIDPath(svc)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return PIDRecord{}, nil
	}
	if err != nil {
		return PIDRecord{}, fmt.Errorf("state: read pid file %s: %w", p, err)
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return PIDRecord{}, fmt.Errorf("state: pid file %s is empty: %w", p, ErrMalformedPID)
	}

	var rec PIDRecord
	if strings.HasPrefix(text, "{") {
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			return PIDRecord{}, fmt.Errorf(
				"state: pid file %s is not a decodable record: %w: %w", p, ErrMalformedPID, err)
		}
	} else {
		pid, convErr := strconv.Atoi(text)
		if convErr != nil {
			return PIDRecord{}, fmt.Errorf("state: pid file %s contains %q: %w: %w", p, text, ErrMalformedPID, convErr)
		}
		rec.PID = pid
	}
	if rec.PID <= 0 {
		return PIDRecord{}, fmt.Errorf("state: pid file %s contains non-positive pid %d: %w", p, rec.PID, ErrMalformedPID)
	}
	return rec, nil
}

// RemovePID deletes svc's pid file. An already-absent file is not an error, so
// removal is safe to call after a process is confirmed dead regardless of who
// won the race to clean up.
func (d *Dir) RemovePID(svc string) error {
	if err := validService(svc); err != nil {
		return err
	}
	p := d.PIDPath(svc)
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("state: remove pid file %s: %w", p, err)
	}
	return nil
}

// TruncateLog rotates svc's previous log to `<log>.1` — overwriting the
// generation before it, so disk use stays bounded at two files per service —
// and returns the fresh log open for writing in append mode with permission
// 0600, ready to be handed to a child process as stdout and stderr. An existing
// log is chmodded back to 0600 so a file created by an older version or a
// different umask cannot leak what the service prints.
//
// The rotation is the point: truncating outright destroyed the previous run's
// output, which is the only evidence a crash leaves behind once the spawning
// process is gone. One generation, not an archive — a developer debugging a
// crash wants the run before the one they just did, not the whole history.
//
// The caller owns the returned file and must Close it once the child has been
// spawned. It returns an error wrapping ErrInvalidService for an unsafe svc, or
// any filesystem error.
func (d *Dir) TruncateLog(svc string) (*os.File, error) {
	if err := validService(svc); err != nil {
		return nil, err
	}
	p := d.LogPath(svc)
	if err := os.Rename(p, p+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("state: rotate log %s: %w", p, err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("state: truncate log %s: %w", p, err)
	}
	if err := f.Chmod(filePerm); err != nil {
		return nil, errors.Join(
			fmt.Errorf("state: chmod log %s to %o: %w", p, filePerm, err),
			f.Close(),
		)
	}
	return f, nil
}

// Reset removes the entire `.dev/` tree, including logs, pid records, exit
// records and the persisted port cache. A missing tree is not an error. Reset
// does not stop any process: killing what the pid files describe is the
// supervisor's job and must happen before the state that names those processes
// is destroyed.
//
// It removes the tree rather than the file families it knows about, so a new
// family under `.dev/` is cleared by construction and cannot accumulate behind
// a `reset` that forgot to list it.
func (d *Dir) Reset() error {
	p := d.Path()
	if err := os.RemoveAll(p); err != nil {
		return fmt.Errorf("state: reset %s: %w", p, err)
	}
	return nil
}

// validService rejects any name that cannot safely compose a file name under
// `.dev/`. It mirrors config's `^[a-zA-Z0-9][a-zA-Z0-9_-]*$` rule; state
// enforces it again as defence in depth, since state is the only package that
// turns a name into a path.
func validService(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name is empty", ErrInvalidService)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			continue
		case (c == '_' || c == '-') && i > 0:
			continue
		}
		return fmt.Errorf(
			"%w: %q must match [a-zA-Z0-9][a-zA-Z0-9_-]* because it composes .dev/logs/<name>.log and .dev/pids/<name>.pid; %q is not allowed",
			ErrInvalidService, name, name[i:i+1])
	}
	return nil
}

// writeFileAtomic writes data to path by way of a temporary file in the same
// directory followed by a rename, so a reader sees either the old contents or
// the new ones and never a partial write. The file mode is set explicitly
// rather than left to the umask.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func(cause error) error {
		return errors.Join(cause, tmp.Close(), os.Remove(tmpName))
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(fmt.Errorf("write %s: %w", tmpName, err))
	}
	if err := tmp.Chmod(perm); err != nil {
		return cleanup(fmt.Errorf("chmod %s to %o: %w", tmpName, perm, err))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(fmt.Errorf("close %s: %w", tmpName, err), os.Remove(tmpName))
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errors.Join(fmt.Errorf("rename %s to %s: %w", tmpName, path, err), os.Remove(tmpName))
	}
	return nil
}
