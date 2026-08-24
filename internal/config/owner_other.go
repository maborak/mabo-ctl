//go:build !unix

package config

// climbableDir allows every directory on platforms mabo-ctl does not supervise.
// The check reads Unix owner and permission bits; there is nothing to read
// here, and failing closed would break discovery for no security gain.
func climbableDir(string) error { return nil }
