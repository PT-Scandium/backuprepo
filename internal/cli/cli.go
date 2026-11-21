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
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"golang.org/x/term"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/backup"
	"backuprepo/internal/daemon"
	"backuprepo/internal/store"
)

// BucketLister looks up buckets from credentials; cfg.BucketName carries the
// bucket being resolved. main.go wires b2.FindBucket, which filters by that name
// so bucket-scoped keys work too. It is passed into Init/Bucket so setup can
// auto-resolve the bucket ID (and so tests can stub the lookup without network
// access). A nil lister disables auto-resolution, leaving the manual prompt.
type BucketLister func(ctx context.Context, cfg b2.Config) ([]b2.BucketInfo, error)

// Init runs interactive setup, reading answers from in and writing prompts to
// out. lister is used to auto-resolve the bucket ID from its name; main.go wires
// b2.FindBucket.
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
	if id, name := resolveBucketID(ctx, lister, cfg.KeyID, cfg.AppKey, cfg.Bucket, "enter it manually", out); id != "" {
		cfg.BucketID, cfg.Bucket = id, name
	}
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
// its ID and B2's spelling of its name, so Init can skip the manual "Bucket ID"
// prompt. B2 bucket names are not case sensitive, so the match folds case and
// the returned name is the one B2 reports (the S3 API wants the real
// spelling). It returns "" for both (and explains why on out, followed by
// followUp) when auto-resolution is unavailable, the lookup fails, or the
// bucket isn't visible to the credentials.
func resolveBucketID(ctx context.Context, lister BucketLister, keyID, appKey, bucket, followUp string, out io.Writer) (id, name string) {
	if lister == nil {
		return "", ""
	}
	// BucketName lets the lookup name the bucket in the request, which B2
	// requires for keys restricted to a single bucket (see b2.FindBucket).
	list, err := lister(ctx, b2.Config{KeyID: keyID, AppKey: appKey, BucketName: bucket})
	if err != nil {
		fmt.Fprintf(out, "Could not auto-resolve bucket ID (%v); %s.\n", err, followUp)
		return "", ""
	}
	for _, bk := range list {
		if strings.EqualFold(bk.Name, bucket) {
			if bk.Name != bucket {
				fmt.Fprintf(out, "Using B2's spelling of the bucket name: %q\n", bk.Name)
			}
			fmt.Fprintf(out, "Resolved bucket ID for %q: %s\n", bk.Name, bk.ID)
			return bk.ID, bk.Name
		}
	}
	fmt.Fprintf(out, "Bucket %q not found in this account (or not visible to this key); %s.\n", bucket, followUp)
	return "", ""
}

// Watch adds an existing directory to the watch list.
//
// The folder is stored as an absolute, cleaned path ("~" expanded), so it
// means the same thing to the daemon and the systemd service (which run in
// $HOME) as it did where `bb watch` was typed. If an equivalent entry already
// exists it is kept as-is: a trailing-slash twin would just walk the same
// files twice, and a legacy relative entry (stored verbatim by older
// versions) names its objects differently, so adding the absolute form would
// upload everything a second time under new names.
func Watch(ctx context.Context, st *store.Store, path string, out io.Writer) error {
	abs, err := normalizeFolder(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s", apperr.ErrFolderNotFound, abs)
	}
	existing, err := findWatched(ctx, st, abs)
	if err != nil {
		return err
	}
	if existing != "" {
		fmt.Fprintf(out, "Already watching %s\n", existing)
		return nil
	}
	if legacy, err := findLegacyRelative(ctx, st, abs); err != nil {
		return err
	} else if legacy != "" {
		fmt.Fprintf(out, "Already watching %s (stored as a relative path by an older bb).\n", legacy)
		fmt.Fprintf(out, "To store it as %s instead, run `bb unwatch %s` then `bb watch %s`;\n", abs, legacy, abs)
		fmt.Fprintln(out, "its files will then be uploaded again under the new, absolute names.")
		return nil
	}
	watched, err := st.ListWatched(ctx)
	if err != nil {
		return err
	}
	own := keySpace(store.Folder{Path: abs})
	for _, f := range watched {
		if f.RemotePrefix == "" {
			continue // plain folders never collide with each other
		}
		if err := checkOverlap(own, f); err != nil {
			return err
		}
	}
	rekeyed, err := countRekeyed(ctx, st, watched, "", abs, "")
	if err != nil {
		return err
	}
	if err := st.AddFolder(ctx, abs); err != nil {
		return err
	}
	fmt.Fprintf(out, "Watching %s\n", abs)
	if rekeyed > 0 {
		fmt.Fprintf(out, "Note: %d file(s) already backed up under a bucket prefix will be uploaded again under their full local paths\n", rekeyed)
		fmt.Fprintln(out, "(objects under the old names are kept).")
	}
	return nil
}

// Unwatch removes a folder from the watch list.
//
// path may be any spelling of a watched folder (trailing slash, relative, "~").
// The exact stored text is tried first, so entries saved verbatim by older
// versions (e.g. relative paths) can still be removed by what `bb list` shows.
// The folder need not exist any more.
func Unwatch(ctx context.Context, st *store.Store, path string, out io.Writer) error {
	err := st.RemoveFolder(ctx, path)
	if err == nil {
		fmt.Fprintf(out, "Stopped watching %s\n", path)
		return warnEnclosing(ctx, st, path, out)
	}
	if !errors.Is(err, apperr.ErrFolderNotWatched) {
		return err
	}
	abs, nerr := normalizeFolder(path)
	if nerr != nil {
		return fmt.Errorf("%w: %s", apperr.ErrFolderNotWatched, path)
	}
	stored, ferr := findWatched(ctx, st, abs)
	if ferr != nil {
		return ferr
	}
	if stored == "" {
		return fmt.Errorf("%w: %s", apperr.ErrFolderNotWatched, path)
	}
	if err := st.RemoveFolder(ctx, stored); err != nil {
		return err
	}
	fmt.Fprintf(out, "Stopped watching %s\n", stored)
	return warnEnclosing(ctx, st, stored, out)
}

// warnEnclosing tells the user, after unwatching dir, when another watched
// folder still contains it: dir's files stay backed up, but under the
// enclosing folder's object names, so they are uploaded again (ADR-027).
func warnEnclosing(ctx context.Context, st *store.Store, dir string, out io.Writer) error {
	watched, err := st.ListWatched(ctx)
	if err != nil {
		return err
	}
	for _, f := range watched {
		if r, err := filepath.Rel(filepath.Clean(f.Path), filepath.Clean(dir)); err == nil &&
			r != "." && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			fmt.Fprintf(out, "Note: %s is still backed up as part of %s, under that folder's object names;\n", dir, f.Path)
			fmt.Fprintln(out, "its files will be uploaded again under those names (objects under the old names are kept).")
			return nil
		}
	}
	return nil
}

// printWatched lists watched folders, one per line, with the bucket prefix of
// any folder that has one.
func printWatched(out io.Writer, watched []store.Folder) {
	for _, f := range watched {
		if f.RemotePrefix != "" {
			fmt.Fprintf(out, "  %s -> %s\n", f.Path, f.RemotePrefix)
		} else {
			fmt.Fprintf(out, "  %s\n", f.Path)
		}
	}
}

// WatchRemote watches the directory path with its files stored under the bucket
// prefix remote instead of under their absolute local paths (ADR-027): with
// remote "MANDALATAMA-SEWA", path/2025/a.pdf is stored as
// "MANDALATAMA-SEWA/2025/a.pdf". If path is already watched its prefix is
// changed; files already backed up are then uploaded again under the new names
// (unless the bucket already holds them there byte-for-byte). A prefix that
// overlaps another folder's object names is refused, since the two folders
// would overwrite each other's objects.
func WatchRemote(ctx context.Context, st *store.Store, path, remote string, out io.Writer) error {
	prefix, err := normalizeRemotePrefix(remote)
	if err != nil {
		return err
	}
	abs, err := normalizeFolder(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s", apperr.ErrFolderNotFound, abs)
	}
	existing, err := findWatched(ctx, st, abs)
	if err != nil {
		return err
	}
	if existing == "" {
		if legacy, err := findLegacyRelative(ctx, st, abs); err != nil {
			return err
		} else if legacy != "" {
			return fmt.Errorf("%w: %s is watched as the relative path %q by an older bb; run `bb unwatch %s` first",
				apperr.ErrInvalidPrefix, abs, legacy, legacy)
		}
	}
	watched, err := st.ListWatched(ctx)
	if err != nil {
		return err
	}
	for _, f := range watched {
		if f.Path == existing {
			if f.RemotePrefix == prefix {
				fmt.Fprintf(out, "Already watching %s -> %s\n", existing, prefix)
				return nil
			}
			continue
		}
		if err := checkOverlap(prefix, f); err != nil {
			return err
		}
	}
	rekeyed, err := countRekeyed(ctx, st, watched, existing, abs, prefix)
	if err != nil {
		return err
	}
	if existing != "" {
		if err := st.SetFolderPrefix(ctx, existing, prefix); err != nil {
			return err
		}
		fmt.Fprintf(out, "Watching %s -> %s (bucket prefix changed)\n", existing, prefix)
	} else {
		if err := st.AddFolderWithPrefix(ctx, abs, prefix); err != nil {
			return err
		}
		fmt.Fprintf(out, "Watching %s -> %s\n", abs, prefix)
	}
	if rekeyed > 0 {
		fmt.Fprintf(out, "Note: %d file(s) already backed up under other names will be uploaded again under %s\n", rekeyed, prefix)
		fmt.Fprintln(out, "(with the b2 backend, files the bucket already holds byte-for-byte are recorded instead; objects under the old names are kept).")
	}
	return nil
}

// keySpace is the object-name prefix every file of folder f is stored under.
func keySpace(f store.Folder) string {
	if f.RemotePrefix != "" {
		return f.RemotePrefix
	}
	return backup.RemoteKey(filepath.Clean(f.Path)) + "/"
}

// checkOverlap refuses names (a folder's key space) that contain, or lie
// inside, the key space of another watched folder f with a bucket prefix, or
// of any folder when names is itself a prefix: the two folders' objects would
// overwrite each other. Two folders without a prefix never collide (their
// names are their distinct absolute paths).
func checkOverlap(names string, f store.Folder) error {
	other := keySpace(f)
	if strings.HasPrefix(names, other) || strings.HasPrefix(other, names) {
		return fmt.Errorf("%w: %q overlaps the object names of watched folder %s (%s)",
			apperr.ErrInvalidPrefix, names, f.Path, other)
	}
	return nil
}

// countRekeyed counts tracked, backed-up files under dir whose object key
// changes once dir is stored under prefix.
func countRekeyed(ctx context.Context, st *store.Store, watched []store.Folder, existing, dir, prefix string) (int, error) {
	files, err := st.ListFiles(ctx)
	if err != nil {
		return 0, err
	}
	after := make([]store.Folder, 0, len(watched)+1)
	for _, f := range watched {
		if f.Path != existing {
			after = append(after, f)
		}
	}
	after = append(after, store.Folder{Path: dir, RemotePrefix: prefix})
	n := 0
	for _, fr := range files {
		if fr.LastBackup == nil {
			continue
		}
		stored := fr.RemoteKey
		if stored == "" {
			stored = backup.RemoteKey(fr.Path) // recorded before keys were stored
		}
		if r, err := filepath.Rel(dir, fr.Path); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			continue
		}
		if backup.KeyFor(after, fr.Path) != stored {
			n++
		}
	}
	return n, nil
}

// maxPrefixLen caps a bucket prefix well under B2's 1024-byte file-name limit,
// leaving room for the files' relative paths.
const maxPrefixLen = 512

// normalizeRemotePrefix turns a --remote argument into a bucket prefix: forward
// slashes, no leading "/", empty segments dropped, ending in "/". It refuses an
// empty prefix (the bucket root would mix with every other folder's objects),
// "." and ".." segments, and control characters (invalid in B2 file names).
func normalizeRemotePrefix(remote string) (string, error) {
	p := strings.ReplaceAll(strings.TrimSpace(remote), "\\", "/")
	var parts []string
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "":
			continue
		case seg == "." || seg == "..":
			return "", fmt.Errorf("%w: %q: \".\" and \"..\" are not allowed", apperr.ErrInvalidPrefix, remote)
		case strings.IndexFunc(seg, unicode.IsControl) >= 0:
			return "", fmt.Errorf("%w: %q: control characters are not allowed", apperr.ErrInvalidPrefix, remote)
		}
		parts = append(parts, seg)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("%w: empty prefix; name a bucket folder, e.g. --remote MANDALATAMA-SEWA", apperr.ErrInvalidPrefix)
	}
	prefix := strings.Join(parts, "/") + "/"
	if len(prefix) > maxPrefixLen {
		return "", fmt.Errorf("%w: %q is longer than %d bytes (B2 file names are limited to 1024)", apperr.ErrInvalidPrefix, remote, maxPrefixLen)
	}
	return prefix, nil
}

// expandHome expands a leading "~", "~/" (or "~\" on Windows) in p using
// home. Other forms (such as "~user") are left alone.
func expandHome(p, home string) string {
	if !isTildePath(p) {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// isTildePath reports whether p is "~" or starts with "~/" (or "~" followed by
// the OS path separator).
func isTildePath(p string) bool {
	return p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~"+string(filepath.Separator))
}

// normalizeFolder returns the absolute, cleaned form of a folder argument,
// expanding a leading "~" (the shell doesn't, e.g. inside --folder=~/x or at
// an interactive prompt). It does not check that the folder exists.
func normalizeFolder(p string) (string, error) {
	// filepath.Abs("") is the current directory: an empty argument (e.g. an
	// unset shell variable in `bb watch "$DIR"`) must never mean "here".
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("%w: empty path", apperr.ErrFolderNotFound)
	}
	if isTildePath(p) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: %s: home dir: %v", apperr.ErrFolderNotFound, p, err)
		}
		p = expandHome(p, home)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", apperr.ErrFolderNotFound, p, err)
	}
	return abs, nil
}

// findLegacyRelative returns a stored relative entry (saved verbatim by bb
// before watch paths were normalised) that resolves to abs from the current
// directory, or "" if none.
func findLegacyRelative(ctx context.Context, st *store.Store, abs string) (string, error) {
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		if filepath.IsAbs(f) {
			continue
		}
		if r, err := filepath.Abs(f); err == nil && sameFolder(r, abs, runtime.GOOS) {
			return f, nil
		}
	}
	return "", nil
}

// findWatched returns the stored watch entry equivalent to abs (same cleaned
// path; case-insensitive on Windows, e.g. C:\Users\X vs c:\users\x), or ""
// if none. Matching is by path only — it works for folders that no longer
// exist, never stats (so a hung network mount can't block it), and never
// equates a symlink with its target (a symlinked root isn't walked by the
// backup, so watching the real folder must stay possible).
func findWatched(ctx context.Context, st *store.Store, abs string) (string, error) {
	folders, err := st.ListFolders(ctx)
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		if sameFolder(filepath.Clean(f), abs, runtime.GOOS) {
			return f, nil
		}
	}
	return "", nil
}

// sameFolder compares two cleaned absolute paths as the given OS does:
// case-insensitively on Windows (NTFS default), exactly elsewhere. macOS
// volumes may be either, so no folding is guessed there.
func sameFolder(a, b, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
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
	watched, err := st.ListWatched(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Watched folders:")
	printWatched(out, watched)
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

// ServiceProbe reports the background service's state for `bb status` (e.g.
// "active", "inactive", "not installed"), or "" when there is none to report.
type ServiceProbe func(ctx context.Context) string

// Status prints whether configured, the backend, folder and pending-upload
// counts, whether a daemon is running (from the PID file in stateDir), and the
// service state when service is non-nil. The daemon and service lines are
// best-effort: they never fail the command.
func Status(ctx context.Context, st *store.Store, stateDir string, service ServiceProbe, out io.Writer) error {
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
	if pid, ok := daemon.RunningPID(stateDir); ok {
		fmt.Fprintf(out, "Daemon: running (pid %d)\n", pid)
	} else {
		fmt.Fprintln(out, "Daemon: not running")
	}
	if service != nil {
		if state := service(ctx); state != "" {
			fmt.Fprintf(out, "Service: %s\n", state)
		}
	}
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
	watched, err := st.ListWatched(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Watched folders: %d\n", len(folders))
	printWatched(out, watched)
	return nil
}

// Bucket shows the current bucket (no name) or switches to another bucket,
// changing only the bucket name + ID and leaving credentials, endpoint, region,
// and backend untouched. When a name is given without an explicit id, the id is
// auto-resolved from the account via lister (the same lookup init uses, which
// also adopts B2's spelling of the name); if that lookup fails or the bucket
// isn't found, the stored bucket ID is cleared (S3-only) and the output says
// how to set it. An explicit id is always used as-is.
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
		hint := fmt.Sprintf("set it with `bb bucket %s <bucket-id>`", name)
		if rid, rname := resolveBucketID(ctx, lister, cfg.KeyID, cfg.AppKey, name, hint, out); rid != "" {
			id, name = rid, rname
		}
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
	if res.Adopted > 0 {
		fmt.Fprintf(out, "Already in the bucket (recorded, not uploaded): %d\n", res.Adopted)
	}
	if res.HashLookupsFailed > 0 {
		fmt.Fprintf(out, "Could not ask the bucket for %d file(s)' hashes; uploaded them instead\n", res.HashLookupsFailed)
	}
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
