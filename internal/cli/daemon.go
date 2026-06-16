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
func Start(ctx context.Context, st *store.Store, up b2.Uploader, dir string, out io.Writer) error {
	return daemon.New(st, up).Run(ctx, dir, out)
}

// Stop signals a running daemon to shut down gracefully.
func Stop(ctx context.Context, dir string, out io.Writer) error {
	return daemon.Stop(dir, out)
}
