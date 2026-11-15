// Package cli implements backuprepo's subcommand handlers. Handlers take an
// io.Writer (and io.Reader for Init) so they are unit-testable; main.go wires
// the real stdin/stdout and the real uploader.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/backup"
	"backuprepo/internal/store"
)

// BucketLister looks up the account's buckets from credentials. It matches
// b2.ListBuckets and is passed into Init so setup can auto-resolve the bucket ID
// (and so tests can stub the lookup without network access). A nil lister
// disables auto-resolution, leaving the manual bucket-ID prompt.
type BucketLister func(ctx context.Context, cfg b2.Config) ([]b2.BucketInfo, error)

// Init runs interactive setup, reading answers from in and writing prompts to
// out. lister is used to auto-resolve the bucket ID from its name; main.go wires
// b2.ListBuckets.
func Init(ctx context.Context, st *store.Store, in io.Reader, out io.Writer, lister BucketLister) error {
	r := bufio.NewReader(in)
	inputEnded := false // set once the reader is exhausted (EOF)
	ask := func(label string) string {
		fmt.Fprintf(out, "%s: ", label)
		line, err := r.ReadString('\n')
		if err != nil {
			inputEnded = true
		}
		return strings.TrimSpace(line)
	}

	var cfg store.RemoteConfig
	cfg.KeyID = ask("Backblaze keyID (access key ID)")
	// The secret is read without echo on an interactive terminal; from the same
	// buffered reader otherwise, so piped answers stay in order.
	appKey, err := readSecretFrom("Backblaze applicationKey (secret, input hidden): ", r, in, out)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err != nil {
		inputEnded = true
	}
	cfg.AppKey = appKey
	cfg.Bucket = ask("Bucket name")
	if cfg.KeyID == "" || cfg.AppKey == "" || cfg.Bucket == "" {
		// With no interactive stdin (e.g. `! bb init` in an agent shell, or
		// </dev/null) every prompt reads EOF; say so instead of blaming the user.
		if inputEnded {
			fmt.Fprintln(out)
			return apperr.ErrNoInput
		}
		return fmt.Errorf("%w: keyID, applicationKey and bucket are required", apperr.ErrInvalidCredentials)
	}
	// Try to resolve the bucket ID automatically so the user needn't hunt for it;
	// fall back to a manual prompt if the lookup fails or the bucket isn't found.
	cfg.BucketID = resolveBucketID(ctx, lister, cfg.KeyID, cfg.AppKey, cfg.Bucket, out)
	if cfg.BucketID == "" {
		cfg.BucketID = ask("Bucket ID (for native B2 API)")
	}
	cfg.Endpoint = normalizeEndpoint(ask("S3 endpoint URL (e.g. https://s3.us-west-004.backblazeb2.com; blank = native B2 API only)"))
	cfg.Region = ask("S3 region (e.g. us-west-004; blank = derive from endpoint)")
	if cfg.Region == "" && cfg.Endpoint != "" {
		if cfg.Region = regionFromEndpoint(cfg.Endpoint); cfg.Region != "" {
			fmt.Fprintf(out, "Region: %s (from endpoint)\n", cfg.Region)
		}
	}
	// The s3 backend cannot work without an endpoint and region; the native b2
	// backend needs only the bucket ID. Decide before saving anything.
	useB2 := cfg.Endpoint == "" || cfg.Region == ""
	if useB2 && cfg.BucketID == "" {
		return fmt.Errorf("%w: need either an S3 endpoint and region, or a bucket ID for the native B2 API",
			apperr.ErrInvalidCredentials)
	}

	if err := st.SaveConfig(ctx, cfg); err != nil {
		return err
	}
	// SaveConfig keeps the stored backend column, so make sure the backend that
	// will be used can work with what was just saved.
	switch {
	case useB2:
		if err := st.SetBackend(ctx, "b2"); err != nil {
			return err
		}
		if cfg.Endpoint == "" {
			fmt.Fprintln(out, "No S3 endpoint given: using the native B2 backend (`bb backend b2`).")
		} else {
			fmt.Fprintf(out, "No S3 region given and none could be derived from %s: using the native B2 backend (`bb backend b2`).\n",
				cfg.Endpoint)
		}
	case cfg.BucketID == "":
		// A b2 backend left over from an earlier init needs a bucket ID it no
		// longer has; s3 is usable (endpoint + region are set), so switch to it.
		kind, err := st.GetBackend(ctx)
		if err != nil {
			return err
		}
		if kind == "b2" {
			if err := st.SetBackend(ctx, "s3"); err != nil {
				return err
			}
			fmt.Fprintln(out, "No bucket ID for the native B2 backend: switched to the S3 backend (`bb backend s3`).")
		}
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

// resolveBucketID looks the bucket up by name via the native B2 API and returns
// its ID so Init can skip the manual "Bucket ID" prompt. It returns "" (and
// explains why on out) when auto-resolution is unavailable, the lookup fails, or
// the named bucket isn't visible to the credentials — the caller then asks.
func resolveBucketID(ctx context.Context, lister BucketLister, keyID, appKey, bucket string, out io.Writer) string {
	if lister == nil {
		return ""
	}
	list, err := lister(ctx, b2.Config{KeyID: keyID, AppKey: appKey})
	if err != nil {
		fmt.Fprintf(out, "Could not auto-resolve bucket ID (%v); enter it manually.\n", err)
		return ""
	}
	for _, bk := range list {
		if bk.Name == bucket {
			fmt.Fprintf(out, "Resolved bucket ID for %q: %s\n", bucket, bk.ID)
			return bk.ID
		}
	}
	fmt.Fprintf(out, "Bucket %q not found among %d visible bucket(s); enter its ID manually.\n", bucket, len(list))
	return ""
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
		fmt.Fprintln(out, "Status: not configured (run `bb init`)")
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
// and backend untouched. When a name is given without an explicit id, the id is
// auto-resolved from the account via lister (the same lookup init uses); if that
// lookup fails or the bucket isn't found, the stored bucket ID is cleared
// (S3-only). An explicit id is always used as-is.
func Bucket(ctx context.Context, st *store.Store, name, id string, lister BucketLister, out io.Writer) error {
	if name == "" {
		cfg, err := st.GetConfig(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Bucket:    %s\nBucket ID: %s\n", cfg.Bucket, cfg.BucketID)
		return nil
	}
	if id == "" {
		cfg, err := st.GetConfig(ctx)
		if err != nil {
			return err
		}
		id = resolveBucketID(ctx, lister, cfg.KeyID, cfg.AppKey, name, out)
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

// normalizeEndpoint trims the S3 endpoint, adds an https:// scheme when the
// user pasted just the host (as Backblaze's Buckets page shows it), and drops
// any path (e.g. a pasted ".../bucket-name"), since the SDK needs a base URL.
func normalizeEndpoint(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return strings.TrimRight(s, "/")
}

// regionFromEndpoint derives the region from a Backblaze S3 endpoint of the
// form https://s3.<region>.backblazeb2.com, or returns "" if it doesn't match.
func regionFromEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.ToLower(u.Hostname()), ".")
	if len(parts) == 4 && parts[0] == "s3" && parts[1] != "" && parts[2] == "backblazeb2" && parts[3] == "com" {
		return parts[1]
	}
	return ""
}

// readSecret reads a secret from in, writing prompt to out first. When in is an
// interactive terminal the input is read WITHOUT echo so the secret never
// appears on screen or in terminal scrollback; otherwise (piped input, tests) it
// reads a single line. This keeps `pass show … | bb appkey` and the unit tests
// working while hiding the secret during interactive entry.
func readSecret(prompt string, in io.Reader, out io.Writer) (string, error) {
	s, err := readSecretFrom(prompt, bufio.NewReader(in), in, out)
	if errors.Is(err, io.EOF) {
		err = nil // a final line without a newline (or empty input) is still an answer
	}
	return s, err
}

// readSecretFrom is readSecret for callers that read several answers through
// one buffered reader r wrapping in (e.g. Init). On an interactive terminal it
// reads without echo; otherwise it reads the next line from r, so piped
// answers stay in order (a second reader on in would find them already
// buffered by r). The r.Buffered() check is defensive: a canonical-mode
// terminal returns one line per read, so r rarely holds typeahead there, but
// if it does, those bytes must be read from r rather than skipped.
// A piped read that reaches end of input returns io.EOF with whatever was read.
// On a terminal, x/term ignores Ctrl-D, so io.EOF comes only from piped input.
func readSecretFrom(prompt string, r *bufio.Reader, in io.Reader, out io.Writer) (string, error) {
	fmt.Fprint(out, prompt)
	if f, ok := in.(*os.File); ok && r.Buffered() == 0 && term.IsTerminal(int(f.Fd())) {
		return readHidden(int(f.Fd()), out)
	}
	line, err := r.ReadString('\n')
	return strings.TrimSpace(line), err
}

// readHidden reads one line from terminal fd without echo. term.ReadPassword
// restores echo in a defer, which never runs if Ctrl-C kills the process
// mid-read — leaving the user's terminal (or Windows console) not echoing.
// So while the read is in progress, an interrupt restores the saved terminal
// state and exits with the conventional 130; the blocked read() cannot be
// cancelled, so exiting is the only way out.
func readHidden(fd int, out io.Writer) (string, error) {
	if state, err := term.GetState(fd); err == nil {
		sig := make(chan os.Signal, 1)
		done := make(chan struct{})
		signal.Notify(sig, os.Interrupt)
		go func() {
			select {
			case <-sig:
				_ = term.Restore(fd, state)
				fmt.Fprintln(out)
				os.Exit(130)
			case <-done:
			}
		}()
		defer func() {
			signal.Stop(sig)
			close(done)
		}()
	}
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(out) // ReadPassword swallows the Enter keypress; emit the newline
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
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
