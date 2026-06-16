// CLI handlers for the background daemon (start/stop). These are thin wrappers
// over internal/daemon, mirroring how Upload wraps internal/backup; main.go
// wires the real uploader, backup_repo dir, and stdout.
package cli

import (
	"context"
	"io"

	"backuprepo/internal/b2"
	"backuprepo/internal/daemon"
	"backuprepo/internal/store"
)

// Start runs the file-watching daemon in the foreground until stopped. dir is
// ~/backup_repo (for the PID file). It blocks until Ctrl-C or `backuprepo stop`.
// When deleteRemoved is set, the daemon also removes remote objects whose local
// files were deleted (opt-in; destructive).
func Start(ctx context.Context, st *store.Store, be b2.Backend, deleteRemoved bool, dir string, out io.Writer) error {
	d := daemon.New(st, be)
	if deleteRemoved {
		d.EnableDeletions(be)
	}
	return d.Run(ctx, dir, out)
}

// Stop signals a running daemon to shut down gracefully.
func Stop(ctx context.Context, dir string, out io.Writer) error {
	return daemon.Stop(dir, out)
}
