package config

import (
	"crypto/sha256"
	"fmt"
	"os"
)

// fingerprint hashes the configuration's sources: the bytes yaml was parsed
// from, then every other path in order, each prefixed with its name and length
// so moving text from one file into another still changes the sum.
//
// yaml is passed in rather than re-read so a Config's baseline is the document
// that was actually parsed. A baseline taken by a second read after the fact
// would absorb an edit made in between, and that edit would never be offered
// for reload.
func fingerprint(path string, yaml []byte, others []string) ([32]byte, error) {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%d\x00", path, len(yaml))
	_, _ = h.Write(yaml)
	for _, p := range others {
		b, err := os.ReadFile(p)
		if err != nil {
			return [32]byte{}, fmt.Errorf("config: read %s: %w", p, err)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		_, _ = h.Write(b)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// envFiles lists the distinct env_file paths the declared services name, in
// declaration order. It runs at Load time, before [Config.ApplyProfiles] can
// filter Services, so the set it records is the same set every later
// [Config.Changed] call re-reads.
func (c *Config) envFiles() []string {
	var out []string
	seen := make(map[string]bool)
	for _, s := range c.Services {
		p := s.EnvFilePath(c.Root)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// Changed reports whether mabo-ctl.yaml, or any env_file it names, now differs
// from what this Config was loaded from. An env_file counts because its
// contents become the service's environment at resolve time: an edit to it
// changes what a restart would run exactly as an edit to the YAML does.
//
// A source that can no longer be read reports changed together with the read
// error, because a configuration that cannot be re-read is certainly not the
// one in force. A Config that was not produced by [Load] has no sources and
// never reports a change.
func (c *Config) Changed() (bool, error) {
	if c == nil || c.Path == "" || c.revision == ([32]byte{}) {
		return false, nil
	}
	yaml, err := os.ReadFile(c.Path)
	if err != nil {
		return true, fmt.Errorf("config: read %s: %w", c.Path, err)
	}
	now, err := fingerprint(c.Path, yaml, c.sources)
	if err != nil {
		return true, err
	}
	return now != c.revision, nil
}
