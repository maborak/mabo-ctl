//go:build unix

package config

import (
	"fmt"
	"os"
	"syscall"
)

// climbableDir reports why a directory reached by climbing may not be trusted
// to hold a mabo-ctl.yaml, or nil when it may.
//
// Two refusals. A directory writable by group or world is one somebody else can
// drop a config into — /tmp is the obvious one, a shared build area the
// realistic one. A directory owned by another user is theirs, not the
// operator's. Either way mabo-ctl would be executing commands chosen by whoever
// owns that directory rather than by the person who ran it.
//
// Anything it cannot determine — a platform with no Unix owner information —
// is allowed through: this narrows an existing behaviour, and failing closed on
// a stat mabo-ctl could not perform would break discovery for no security gain.
func climbableDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	if mode := fi.Mode().Perm(); mode&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or world (mode %#o)", dir, mode)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		return fmt.Errorf("%s is owned by uid %d, not by you (uid %d)", dir, st.Uid, uid)
	}
	return nil
}
