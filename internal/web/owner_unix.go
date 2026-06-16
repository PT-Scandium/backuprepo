//go:build !windows

package web

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// fileOwner returns the OS login name of the file's owner, falling back to the
// numeric uid (or "—" when unavailable).
func fileOwner(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "—"
	}
	uid := strconv.FormatUint(uint64(st.Uid), 10)
	if u, err := user.LookupId(uid); err == nil && u.Username != "" {
		return u.Username
	}
	return uid
}
