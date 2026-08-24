//go:build !unix

package supervisor

// signalIgnoreTERM is a no-op on platforms mabo-ctl does not supervise.
func signalIgnoreTERM() {}
