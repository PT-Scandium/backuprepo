//go:build windows

package web

import "os"

// fileOwner is a stub on Windows: file ownership isn't exposed via os.FileInfo,
// and resolving it needs Windows security APIs (a future enhancement). Show a
// placeholder rather than a wrong value.
func fileOwner(fi os.FileInfo) string {
	_ = fi
	return "—"
}
