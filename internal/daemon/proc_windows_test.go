//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsProcStartTimeStable(t *testing.T) {
	a, ok := procStartTime(os.Getpid())
	if !ok || a == "" {
		t.Fatalf("procStartTime(self) = %q, %v", a, ok)
	}
	if b, _ := procStartTime(os.Getpid()); b != a {
		t.Errorf("start time changed: %q then %q", a, b)
	}
	if !processAlive(os.Getpid()) {
		t.Error("processAlive(self) = false")
	}
}

// TestWindowsExitedProcessNotAlive: a Windows process object survives while
// any handle to it is open, so after the child exits this test keeps its own
// handle open — processAlive must still report false (exit code is no longer
// STILL_ACTIVE), which a bare "can I open it?" check would get wrong.
func TestWindowsExitedProcessNotAlive(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cmd unavailable: %v", err)
	}
	pid := cmd.Process.Pid
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		_ = cmd.Wait()
		t.Skipf("open child: %v", err)
	}
	defer windows.CloseHandle(h)
	if ev, err := windows.WaitForSingleObject(h, 10000); err != nil || ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("child did not exit within 10s (event %#x, err %v)", ev, err)
	}
	if processAlive(pid) {
		t.Errorf("exited pid %d reported alive while a handle is still open", pid)
	}
	_ = cmd.Wait()
}
