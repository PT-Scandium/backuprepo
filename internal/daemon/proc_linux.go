//go:build linux

package daemon

import (
	"os"
	"strconv"
)

// procStartTime returns pid's start time (clock ticks since boot, field 22 of
// /proc/<pid>/stat). Together with the PID it identifies one specific process:
// a PID reused by another process has a different start time.
func procStartTime(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	return parseStatStartTime(string(data))
}
