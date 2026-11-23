package syncer

import (
	"runtime"
	"strings"
)

// samePath compares two cleaned paths as the OS does: case-insensitively on
// Windows (NTFS default), exactly elsewhere.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
