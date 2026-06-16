// Package cli implements backuprepo's subcommand handlers. Handlers take an
// io.Writer (and io.Reader for Init) so they are unit-testable; main.go wires
// the real stdin/stdout and the real uploader.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/backup"
	"backuprepo/internal/store"
)

// Init runs interactive setup, reading answers from in and writing prompts to out.
func Init(ctx context.Context, st *store.Store, in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	ask := func(label string) string {
		fmt.Fprintf(out, "%s: ", label)
		line, _ := r.ReadString('\n')
		return strings.TrimSpace(line)
	}

	cfg := store.RemoteConfig{
		KeyID:    ask("Backblaze keyID (access key ID)"),
		AppKey:   ask("Backblaze applicationKey (secret)"),
		Bucket:   ask("Bucket name"),
		BucketID: ask("Bucket ID (for native B2 API)"),
		Endpoint: ask("S3 endpoint URL (e.g. https://s3.us-west-004.backblazeb2.com)"),
		Region:   ask("S3 region (e.g. us-west-004)"),
	}
	if cfg.KeyID == "" || cfg.AppKey == "" || cfg.Bucket == "" {
		return fmt.Errorf("%w: keyID, applicationKey and bucket are required", apperr.ErrInvalidCredentials)
	}
	if err := st.SaveConfig(ctx, cfg); err != nil {
		return err
	}
	fmt.Fprintln(out, "Configuration saved.")

	folder := ask("Folder to watch (blank to skip)")
	if folder != "" {
		if err := Watch(ctx, st, folder, out); err != nil {
			return err
		}
	}
	return nil
}

// Watch adds an existing directory to the watch list.
func Watch(ctx context.Context, st *store.Store, path string, out io.Writer) error {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s", apperr.ErrFolderNotFound, path)
	}
	if err := st.AddFolder(ctx, path); err != nil {
		return err
	}
	fmt.Fprintf(out, "Watching %s\n", path)
	return nil
}

// Unwatch removes a folder from the watch list.
func Unwatch(ctx context.Context, st *store.Store, path string, out io.Writer) error {
	if err := st.RemoveFolder(ctx, path); err != nil {
		return err
	}
	fmt.Fprintf(out, "Stopped watching %s\n", path)
	return nil
}

// List prints watched folders and tracked files with backup state.
func List(ctx context.Context, st *store.Store, out io.Writer) error {
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return err
	}
	if len(folders) == 0 {
		fmt.Fprintln(out, "No folders are being watched.")
		return nil
	}
	fmt.Fprintln(out, "Watched folders:")
	for _, f := range folders {
		fmt.Fprintf(out, "  %s\n", f)
	}
	files, err := st.ListFiles(ctx)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	fmt.Fprintln(out, "\nTracked files:")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tSIZE\tLAST BACKUP")
	for _, fr := range files {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", fr.Path, fr.Size, lastBackup(fr.LastBackup))
	}
	return tw.Flush()
}

// Status prints whether configured, folder count, and pending upload count.
func Status(ctx context.Context, st *store.Store, out io.Writer) error {
	configured, err := st.IsConfigured(ctx)
	if err != nil {
		return err
	}
	if !configured {
		fmt.Fprintln(out, "Status: not configured (run `backuprepo init`)")
		return nil
	}
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return err
	}
	svc := backup.New(st, b2.NewFake()) // PendingCount does not upload
	pending, err := svc.PendingCount(ctx)
	if err != nil {
		return err
	}
	backend, err := st.GetBackend(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Status: configured\nBackend: %s\nWatched folders: %d\nPending uploads: %d\n",
		backend, len(folders), pending)
	return nil
}

// Config prints the current config with the secret masked.
func Config(ctx context.Context, st *store.Store, out io.Writer) error {
	cfg, err := st.GetConfig(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Endpoint:    %s\n", cfg.Endpoint)
	fmt.Fprintf(out, "Region:      %s\n", cfg.Region)
	fmt.Fprintf(out, "Bucket:      %s\n", cfg.Bucket)
	fmt.Fprintf(out, "Bucket ID:   %s\n", cfg.BucketID)
	backend, err := st.GetBackend(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Backend:     %s\n", backend)
	fmt.Fprintf(out, "Key ID:      %s\n", cfg.KeyID)
	fmt.Fprintf(out, "App Key:     %s\n", mask(cfg.AppKey))
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Watched folders: %d\n", len(folders))
	for _, f := range folders {
		fmt.Fprintf(out, "  %s\n", f)
	}
	return nil
}

// Bucket shows the current bucket (no name) or switches to another bucket,
// changing only the bucket name + ID and leaving credentials, endpoint, region,
// and backend untouched. An empty id clears the stored bucket ID (S3-only).
func Bucket(ctx context.Context, st *store.Store, name, id string, out io.Writer) error {
	if name == "" {
		cfg, err := st.GetConfig(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Bucket:    %s\nBucket ID: %s\n", cfg.Bucket, cfg.BucketID)
		return nil
	}
	if err := st.SetBucket(ctx, name, id); err != nil {
		return err
	}
	if id == "" {
		fmt.Fprintf(out, "Bucket set to %s (bucket ID cleared)\n", name)
	} else {
		fmt.Fprintf(out, "Bucket set to %s (id %s)\n", name, id)
	}
	return nil
}

// SetAppKey replaces the stored applicationKey (secret), read from in as a
// single line so the secret never appears in argv or shell history. When
// newKeyID is non-empty the stored keyID is updated too (B2 keys are
// keyID+secret pairs). Endpoint, region, bucket, and backend are untouched.
func SetAppKey(ctx context.Context, st *store.Store, newKeyID string, in io.Reader, out io.Writer) error {
	ok, err := st.IsConfigured(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.ErrNotConfigured
	}
	appKey, err := readSecret("New applicationKey (input hidden; or pipe via stdin): ", in, out)
	if err != nil {
		return err
	}
	if appKey == "" {
		return fmt.Errorf("%w: empty applicationKey", apperr.ErrInvalidCredentials)
	}
	if err := st.SetCredentials(ctx, newKeyID, appKey); err != nil {
		return err
	}
	if newKeyID != "" {
		fmt.Fprintf(out, "Key ID set to %s\n", newKeyID)
	}
	fmt.Fprintf(out, "Application key updated (%s)\n", mask(appKey))
	return nil
}

// Upload force-scans watched folders and uploads changed files. When
// deleteRemoved is set, it also removes remote objects whose local files were
// deleted (opt-in; destructive).
func Upload(ctx context.Context, st *store.Store, be b2.Backend, deleteRemoved bool, out io.Writer) error {
	svc := backup.New(st, be)
	if deleteRemoved {
		svc = svc.WithDeleter(be)
	}
	res, err := svc.UploadChanged(ctx)
	fmt.Fprintf(out, "Uploaded: %d, Skipped: %d, Deleted: %d, Failed: %d\n",
		res.Uploaded, res.Skipped, res.Deleted, res.Failed)
	return err
}

// readSecret reads a secret from in, writing prompt to out first. When in is an
// interactive terminal the input is read WITHOUT echo so the secret never
// appears on screen or in terminal scrollback; otherwise (piped input, tests) it
// reads a single line. This keeps `pass show … | bb appkey` and the unit tests
// working while hiding the secret during interactive entry.
func readSecret(prompt string, in io.Reader, out io.Writer) (string, error) {
	fmt.Fprint(out, prompt)
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out) // ReadPassword swallows the Enter keypress; emit the newline
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, _ := bufio.NewReader(in).ReadString('\n')
	return strings.TrimSpace(line), nil
}

// mask returns the secret with all but its last four characters hidden.
func mask(secret string) string {
	if len(secret) <= 4 {
		return "****"
	}
	return "****" + secret[len(secret)-4:]
}

// lastBackup formats a Unix timestamp as RFC3339, or "never" when nil.
func lastBackup(ts *int64) string {
	if ts == nil {
		return "never"
	}
	return time.Unix(*ts, 0).Format(time.RFC3339)
}
