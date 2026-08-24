package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
)

// portKeyPrefix is the prefix of every key in run.env that state itself owns.
// Keys that do not start with it belong to some other reader — possibly a
// future version of mabo-ctl — and are preserved verbatim on rewrite.
const portKeyPrefix = "PORT_"

// runEnvHeader is rewritten on every WriteRunEnv. It is a comment, so it is
// dropped by the parser and never mistaken for a preserved key.
const runEnvHeader = "# mabo-ctl resolved ports. Generated file; safe to delete.\n"

// RunEnv is the parsed contents of `.dev/run.env`, the persisted resolved-port
// cache. It is a plain KEY=VALUE file whose port keys are `PORT_<SERVICE>`.
//
// A persisted port OUTRANKS the default declared in mabo-ctl.yaml, which makes
// this file a documented trap: editing a default in mabo-ctl.yaml changes nothing
// until run.env is cleared. service.Resolve is required to flag that case, so
// nothing here silently prefers stale state.
//
// Keys the file carries that are not `PORT_*` are kept and written back
// unchanged, so a field added by a future version of mabo-ctl survives a rewrite
// by an older one rather than being silently dropped.
type RunEnv struct {
	// Ports maps a service name to its persisted port. The key is the service
	// name canonicalised to lower case with `-` replaced by `_`, which is the
	// inverse of the `PORT_<UPPER_SERVICE_NAME>` file key. For the ordinary
	// lower-case service name that means the declared name itself; use Port to
	// look up a name whose spelling differs from the canonical form.
	Ports map[string]int

	// unknown holds every non-PORT_ key in the order the file listed it, so a
	// rewrite reproduces the file's own ordering.
	unknown []rawEntry
	// malformed counts lines that could not be parsed and were skipped.
	malformed int
}

// rawEntry is one preserved KEY=VALUE pair that state does not interpret.
type rawEntry struct {
	key   string
	value string
}

// Port returns the persisted port for the service named n and whether one was
// recorded. It matches n exactly first and then by canonical form, so a service
// named "browser-service" finds the port stored under `PORT_BROWSER_SERVICE`.
func (r *RunEnv) Port(n string) (int, bool) {
	if r == nil || len(r.Ports) == 0 {
		return 0, false
	}
	if p, ok := r.Ports[n]; ok {
		return p, true
	}
	want := canonicalService(n)
	for k, p := range r.Ports {
		if canonicalService(k) == want {
			return p, true
		}
	}
	return 0, false
}

// SetPort records port for the service named n, allocating Ports if the RunEnv
// was constructed as a literal with a nil map.
func (r *RunEnv) SetPort(n string, port int) {
	if r.Ports == nil {
		r.Ports = make(map[string]int, 4)
	}
	r.Ports[n] = port
}

// Unknown returns a copy of the keys run.env carried that state does not
// interpret. They are preserved across a rewrite; the copy means a caller
// cannot mutate what will be written back.
func (r *RunEnv) Unknown() map[string]string {
	out := make(map[string]string, len(r.unknown))
	for _, e := range r.unknown {
		out[e.key] = e.value
	}
	return out
}

// Malformed reports how many lines of run.env could not be parsed and were
// skipped. Junk in the cache is never fatal — a bad line must not stop mabo-ctl
// from starting a service — but a caller that wants to warn about a corrupt
// cache can use this count.
func (r *RunEnv) Malformed() int { return r.malformed }

// ReadRunEnv loads `.dev/run.env`.
//
// A missing file yields an empty RunEnv and no error: nothing has been resolved
// yet, which is a normal first-run state. Unparseable lines and `PORT_*` keys
// whose value is not a port number are skipped and counted in Malformed rather
// than failing the read. It returns an error only when the file exists but
// cannot be read.
func (d *Dir) ReadRunEnv() (*RunEnv, error) {
	re := &RunEnv{Ports: make(map[string]int, 4)}
	p := d.RunEnvPath()
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return re, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: read %s: %w", p, err)
	}
	re.parse(string(b))
	return re, nil
}

// parse fills re from the raw contents of a run.env file. Blank lines and
// comments are ignored; everything else is either a port key, a preserved
// unknown key, or malformed.
func (r *RunEnv) parse(text string) {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			r.malformed++
			continue
		}
		key := strings.TrimSpace(line[:eq])
		value := unquote(strings.TrimSpace(line[eq+1:]))
		if key == "" {
			r.malformed++
			continue
		}
		if !strings.HasPrefix(key, portKeyPrefix) || len(key) == len(portKeyPrefix) {
			r.setUnknown(key, value)
			continue
		}
		port, err := strconv.Atoi(value)
		if err != nil || port < 0 || port > 65535 {
			// A PORT_ key with a junk value is dropped rather than preserved:
			// keeping it would collide with the value this service resolves on
			// the next write and produce a duplicate key in the file.
			r.malformed++
			continue
		}
		r.Ports[serviceFromPortKey(key)] = port
	}
}

// setUnknown records a preserved key, replacing an earlier occurrence in place
// so the file's original ordering survives a duplicate.
func (r *RunEnv) setUnknown(key, value string) {
	for i := range r.unknown {
		if r.unknown[i].key == key {
			r.unknown[i].value = value
			return
		}
	}
	r.unknown = append(r.unknown, rawEntry{key: key, value: value})
}

// WriteRunEnv persists re to `.dev/run.env` atomically with mode 0600.
//
// Port keys are written sorted so the file has a stable diff. Unknown keys are
// preserved: those carried by re, unioned with any still on disk that re does
// not know about, so a rewrite by a caller that built a RunEnv from scratch
// still cannot drop a field written by a future version of mabo-ctl.
//
// It returns an error when re is nil, when a key in re.Ports is not a valid
// service name, when two Ports keys collide on the same `PORT_` file key with
// different values (which would silently lose one of them), or on any
// filesystem error.
func (d *Dir) WriteRunEnv(re *RunEnv) error {
	if re == nil {
		return errors.New("state: WriteRunEnv: nil RunEnv")
	}
	keys := make(map[string]string, len(re.Ports)) // file key -> service name
	names := make([]string, 0, len(re.Ports))
	for name := range re.Ports {
		if err := validService(name); err != nil {
			return fmt.Errorf("state: WriteRunEnv: %w", err)
		}
		fk := portKey(name)
		if other, dup := keys[fk]; dup && re.Ports[other] != re.Ports[name] {
			return fmt.Errorf(
				"state: WriteRunEnv: services %q and %q both map to run.env key %s with different ports (%d and %d)",
				other, name, fk, re.Ports[other], re.Ports[name])
		}
		keys[fk] = name
		names = append(names, name)
	}
	sort.Strings(names)

	// Read-modify-write under a lock. preservedKeys READS the file that the
	// write below replaces, so the two steps are one operation: without the
	// lock, two mabo-ctl invocations both read, both merge onto the same stale
	// snapshot, and the second write silently discards the first one's ports.
	return withFileLock(d.RunEnvLockPath(), func() error {
		preserved, err := d.preservedKeys(re)
		if err != nil {
			return err
		}

		var b strings.Builder
		b.WriteString(runEnvHeader)
		written := make(map[string]bool, len(names))
		for _, name := range names {
			fk := portKey(name)
			if written[fk] {
				continue
			}
			written[fk] = true
			fmt.Fprintf(&b, "%s=%d\n", fk, re.Ports[name])
		}
		for _, e := range preserved {
			fmt.Fprintf(&b, "%s=%s\n", e.key, e.value)
		}

		p := d.RunEnvPath()
		if err := writeFileAtomic(p, []byte(b.String()), filePerm); err != nil {
			return fmt.Errorf("state: write %s: %w", p, err)
		}
		return nil
	})
}

// preservedKeys returns the unknown keys to write back: what is on disk now,
// overlaid with what re carries. Re-reading the file means a caller that never
// called ReadRunEnv cannot drop another writer's key.
func (d *Dir) preservedKeys(re *RunEnv) ([]rawEntry, error) {
	onDisk, err := d.ReadRunEnv()
	if err != nil {
		return nil, fmt.Errorf("state: WriteRunEnv: read existing keys: %w", err)
	}
	merged := make([]rawEntry, len(onDisk.unknown))
	copy(merged, onDisk.unknown)
	for _, e := range re.unknown {
		replaced := false
		for i := range merged {
			if merged[i].key == e.key {
				merged[i].value = e.value
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, e)
		}
	}
	return merged, nil
}

// portKey returns the run.env key for a service name: `PORT_` followed by the
// name upper-cased with `-` replaced by `_`, so the key is a usable shell
// variable name.
func portKey(name string) string {
	return portKeyPrefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// serviceFromPortKey is the inverse of portKey for the canonical spelling of a
// service name. `-` cannot be recovered from `_`, which is why RunEnv.Port
// compares canonical forms instead of relying on an exact map hit.
func serviceFromPortKey(key string) string {
	return strings.ToLower(strings.TrimPrefix(key, portKeyPrefix))
}

// canonicalService folds a service name to the spelling used as a RunEnv.Ports
// key, so two spellings that share a run.env key compare equal.
func canonicalService(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "-", "_"))
}

// unquote strips one layer of matching surrounding quotes, so a value written
// by hand as PORT_X="7100" still parses.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
