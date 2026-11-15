//go:build !linux && !windows

package daemon

// procStartTime is unavailable here (no /proc, not Windows: e.g. macOS), so
// PID-file liveness falls back to processAlive alone.
func procStartTime(int) (string, bool) { return "", false }
