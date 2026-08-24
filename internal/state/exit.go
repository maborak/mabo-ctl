package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ErrMalformedExit is returned by Dir.ReadExit when an exit record exists but
// cannot be decoded. An absent record is not an error — it means "mabo-ctl has
// not seen this service die" — but an unreadable one is: silently treating a
// corrupt record as absent would report a crashed service as never started,
// which is the exact confusion the record exists to remove.
var ErrMalformedExit = errors.New("malformed exit record")

// ExitRecord is mabo-ctl's persisted account of one supervised process's death:
// what died, how it died, when it ran, and what it printed on the way out.
//
// It exists because the process that observes a death is almost never the
// process that has to report it. The supervisor watches a child exit and then
// exits itself; the next `mabo-ctl status` is a fresh process with no memory of
// either. Without a record on disk, a service that crashed with a stack trace
// two seconds ago is byte-identical to one that was never started.
//
// A record is written when mabo-ctl observes a death and removed when mabo-ctl
// starts or deliberately stops the service, so at most one record exists per
// service and it always describes the most recent run.
type ExitRecord struct {
	// PID is the process id that died. It identifies the run the record
	// describes, and is not safe to signal: by definition that process is gone
	// and the number may already have been recycled.
	PID int `json:"pid"`
	// ExitCode is the status the process exited with, or -1 when a signal
	// killed it and there is no exit status to report.
	ExitCode int `json:"exit_code"`
	// Signal is the name of the signal that killed the process ("SIGKILL"), or
	// empty when the process exited on its own and ExitCode is authoritative.
	Signal string `json:"signal"`
	// StartedAt is when mabo-ctl spawned the process, copied from the pid record
	// so uptime-at-death is renderable without a second file.
	StartedAt time.Time `json:"started_at"`
	// EndedAt is when mabo-ctl observed the death. It is the timestamp "4m ago"
	// is computed from.
	EndedAt time.Time `json:"ended_at"`
	// LogTail is the last few lines the process printed before dying — the
	// stack trace or bind error that explains the death. It is capped by the
	// writer, and it is why exit records are mode 0600 like every other file
	// under `.dev/`: whatever a service prints can include a credential.
	LogTail string `json:"log_tail"`
	// Stopped reports whether mabo-ctl took the process down deliberately. A
	// deliberate stop is not a crash, and conflating the two would make every
	// clean shutdown look like a failure.
	Stopped bool `json:"stopped"`
	// Startup reports that the process died while mabo-ctl was still waiting for
	// it to become ready — i.e. it never came up at all. It is what separates
	// the "failed" phase from "exited": a service that has never worked once is
	// a different problem from one that ran all morning and crashed at three,
	// and the process that has to say which is not the process that watched it
	// happen. An older record without the field decodes as false and reads as
	// "exited", which is the safe direction: it claims less, not more.
	Startup bool `json:"startup"`
}

// ExitsDir returns the absolute path of the exit-record directory
// (Root/.dev/exits).
func (d *Dir) ExitsDir() string { return filepath.Join(d.Path(), exitsDirName) }

// ExitPath returns the exit record path for svc: Root/.dev/exits/<svc>.json. It
// does not validate svc; callers that create or read the file go through
// methods that do. Passing an unvalidated name here can escape `.dev/`.
func (d *Dir) ExitPath(svc string) string {
	return filepath.Join(d.ExitsDir(), svc+".json")
}

// WriteExit records r as svc's most recent death, replacing any earlier record.
// The file is written atomically (temp file plus rename) with mode 0600, so a
// concurrent ReadExit never observes a half-written record and the log tail
// inside it is never group- or world-readable.
//
// It returns an error wrapping ErrInvalidService when svc is not a safe name,
// or any encoding or filesystem error.
func (d *Dir) WriteExit(svc string, r ExitRecord) error {
	if err := validService(svc); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("state: encode exit record for %q: %w", svc, err)
	}
	p := d.ExitPath(svc)
	if err := writeFileAtomic(p, append(b, '\n'), filePerm); err != nil {
		return fmt.Errorf("state: write exit record %s: %w", p, err)
	}
	return nil
}

// ReadExit returns svc's most recent exit record and whether one exists.
//
// An absent record returns (zero, false, nil): "mabo-ctl has never seen this
// service die" is a normal state, not a failure, and it is what a service that
// is running or was never started looks like. A record that exists but cannot
// be decoded returns an error wrapping ErrMalformedExit, matching ReadPID's
// treatment of a corrupt pid file — a file mabo-ctl wrote and cannot read back is
// a bug or tampering, and reporting it as "no crash" would hide both.
//
// Only the record's syntax is validated. Unlike a pid, no field here is ever
// signalled or otherwise acted on, so a semantically odd record is rendered as
// written rather than rejected.
func (d *Dir) ReadExit(svc string) (ExitRecord, bool, error) {
	if err := validService(svc); err != nil {
		return ExitRecord{}, false, err
	}
	p := d.ExitPath(svc)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ExitRecord{}, false, nil
	}
	if err != nil {
		return ExitRecord{}, false, fmt.Errorf("state: read exit record %s: %w", p, err)
	}
	var r ExitRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return ExitRecord{}, false, fmt.Errorf(
			"state: exit record %s is not decodable: %w: %w", p, ErrMalformedExit, err)
	}
	return r, true, nil
}

// RemoveExit deletes svc's exit record. An already-absent record is not an
// error, so a caller can clear the record unconditionally before a start or
// after a deliberate stop without first asking whether one is there.
//
// Removing on a clean stop is what keeps a stopped service from masquerading as
// a crashed one.
func (d *Dir) RemoveExit(svc string) error {
	if err := validService(svc); err != nil {
		return err
	}
	p := d.ExitPath(svc)
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("state: remove exit record %s: %w", p, err)
	}
	return nil
}
