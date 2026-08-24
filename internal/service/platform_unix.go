//go:build unix

package service

import "io/fs"

// checkPlatform reports whether this build can resolve service runtimes. On a
// unix system it always can.
func checkPlatform() error { return nil }

// executableMode reports whether m carries any execute bit. Any of the three is
// enough: whether mabo-ctl may actually run the file depends on ownership and
// group membership, which os.Stat does not answer, so the check is deliberately
// permissive and lets exec report a definitive EACCES.
func executableMode(m fs.FileMode) bool { return m&0o111 != 0 }
