//go:build windows

package daemon

import (
	"strconv"

	"golang.org/x/sys/windows"
)

// procStartTime returns pid's creation time (from GetProcessTimes, in 100 ns
// units since the Unix epoch via Filetime.Nanoseconds) — the Windows counterpart of Linux's /proc start time.
// Together with the PID it identifies one specific process: a PID reused by
// another process has a different creation time.
func procStartTime(pid int) (string, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return "", false
	}
	return strconv.FormatInt(creation.Nanoseconds()/100, 10), true
}
