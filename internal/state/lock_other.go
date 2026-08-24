//go:build !unix

package state

// withFileLock runs fn unlocked on platforms mabo-ctl does not supervise. Nothing
// on this platform ever spawns a service, so there is no second mabo-ctl to race
// with; the unix build carries the real implementation.
func withFileLock(_ string, fn func() error) error { return fn() }
