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
// ~/backup_repo (for the PID file). It blocks until Ctrl-C or `bb stop`.
// When deleteRemoved is set, the daemon also removes remote objects whose local
// files were deleted (opt-in; destructive).
//
// backendKind is the effective backend ("s3" or "b2"): mapped folders are kept
// in two-way sync (ADR-028) on every pass, which needs "b2"; losing local
// versions of a conflict go to dir/conflicts.
func Start(ctx context.Context, st *store.Store, be b2.Backend, backendKind string, deleteRemoved bool, dir string, out io.Writer) error {
	d := daemon.New(st, be)
	if deleteRemoved {
		d.EnableDeletions(be)
	}
	if backendKind == "b2" {
		d.EnableSync(func(ctx context.Context, out io.Writer) error {
			return SyncMapped(ctx, st, be, backendKind, dir, out)
		})
	} else {
		// Warn once at startup rather than on every pass.
		_ = SyncMapped(ctx, st, be, backendKind, dir, out)
	}
	return d.Run(ctx, dir, out)
}

// Stop signals a running daemon to shut down gracefully.
func Stop(ctx context.Context, dir string, out io.Writer) error {
	return daemon.Stop(dir, out)
}
