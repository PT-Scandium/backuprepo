//go:build !linux

package daemon

// procStartTime is unavailable off Linux (no /proc), so PID-file liveness
// falls back to processAlive alone there.
func procStartTime(int) (string, bool) { return "", false }
