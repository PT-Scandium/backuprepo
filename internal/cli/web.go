// CLI handler for the localhost web UI (serve). Thin wrapper over internal/web,
// mirroring how Start wraps internal/daemon.
package cli

import (
	"context"
	"io"

	"backuprepo/internal/b2"
	"backuprepo/internal/store"
	"backuprepo/internal/web"
)

// Serve starts the localhost web UI (port 9171) in the foreground until stopped
// (Ctrl-C or the Close button). It does not start the daemon.
//
// backendKind and stateDir configure the Upload button's sync of mapped
// folders (see SyncMapped); its report goes to out (the terminal running
// `bb serve`).
func Serve(ctx context.Context, st *store.Store, be b2.Backend, backendKind, stateDir string, out io.Writer) error {
	cfg, err := st.GetConfig(ctx)
	if err != nil {
		return err
	}
	location := cfg.Bucket
	if cfg.Endpoint != "" {
		location = cfg.Bucket + " @ " + cfg.Endpoint
	}
	srv := web.New(st, be, location)
	srv.SetSync(func(ctx context.Context) error {
		return SyncMapped(ctx, st, be, backendKind, stateDir, out)
	})
	return srv.Serve(ctx, out)
}
