//go:build darwin || linux

package supervisor

import (
	"os"
	"syscall"
)

// sameFile reports whether two FileInfo describe the same inode on the same
// device — the identity test that lets a log follower notice its file was
// rotated out from under it.
func sameFile(a, b os.FileInfo) bool {
	ta, oka := a.Sys().(*syscall.Stat_t)
	tb, okb := b.Sys().(*syscall.Stat_t)
	return oka && okb && ta.Dev == tb.Dev && ta.Ino == tb.Ino
}
