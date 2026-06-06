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

	cfg := store.S3Config{
		KeyID:    ask("Backblaze keyID (access key ID)"),
		AppKey:   ask("Backblaze applicationKey (secret)"),
		Bucket:   ask("Bucket name"),
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
	fmt.Fprintf(out, "Status: configured\nWatched folders: %d\nPending uploads: %d\n",
		len(folders), pending)
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

// Upload force-scans watched folders and uploads changed files.
func Upload(ctx context.Context, st *store.Store, up b2.Uploader, out io.Writer) error {
	svc := backup.New(st, up)
	res, err := svc.UploadChanged(ctx)
	fmt.Fprintf(out, "Uploaded: %d, Skipped: %d, Failed: %d\n", res.Uploaded, res.Skipped, res.Failed)
	return err
}

func mask(secret string) string {
	if len(secret) <= 4 {
		return "****"
	}
	return "****" + secret[len(secret)-4:]
}

func lastBackup(ts *int64) string {
	if ts == nil {
		return "never"
	}
	return time.Unix(*ts, 0).Format(time.RFC3339)
}
