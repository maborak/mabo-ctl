//go:build !unix

package supervisor

import "context"

// groupUsage is unavailable off unix; status then carries no usage.
func groupUsage(context.Context) map[int]Usage { return nil }
