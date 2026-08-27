//go:build !darwin && !linux

package supervisor

import "os"

// sameFile cannot compare identities here; returning false makes the follower
// fall back to the size-shrink heuristic only.
func sameFile(_, _ os.FileInfo) bool { return false }
