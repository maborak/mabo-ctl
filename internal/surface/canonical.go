package surface

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// WriteCanonical writes the map with byte-stable formatting: two-space indent,
// trailing newline, sections and ids already sorted by [Enumerate]. Identical
// input must produce identical bytes, or the drift gate in CI argues with git.
func WriteCanonical(m Map, path string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// LoadCanonical reads a committed map for comparison.
func LoadCanonical(path string) (Map, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Map{}, err
	}
	var m Map
	if err := json.Unmarshal(b, &m); err != nil {
		return Map{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// Diff returns human-readable lines for every surface missing from or stale in
// want relative to have. Empty means the maps agree exactly.
func Diff(have, want Map) []string {
	var out []string
	for _, section := range []string{"cli", "config", "json"} {
		live := index(want.Sections[section])
		disk := index(have.Sections[section])
		for id := range live {
			if !disk[id] {
				out = append(out, fmt.Sprintf("+ %s\t(live, missing from map)", id))
			}
		}
		for id := range disk {
			if !live[id] {
				out = append(out, fmt.Sprintf("- %s\t(stale: gone from the binary)", id))
			}
		}
	}
	return out
}

func index(ids []Name) map[Name]bool {
	set := make(map[Name]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// ExecGoBuild compiles ./cmd/mabo-ctl to outPath. Exported for the surfacemap
// tool and the drift test, which both need a fresh binary.
func ExecGoBuild(outPath string) ([]byte, error) {
	return exec.Command("go", "build", "-o", outPath, "github.com/maborak/mabo-ctl/cmd/mabo-ctl").CombinedOutput()
}
