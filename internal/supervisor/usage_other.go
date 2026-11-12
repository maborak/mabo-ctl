//go:build !unix

package supervisor

// groupUsage is unavailable off unix; status then carries no usage.
func groupUsage() map[int]Usage { return nil }
