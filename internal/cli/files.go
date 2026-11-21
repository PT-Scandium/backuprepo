// CLI handlers for manual bucket operations (ls/get/put/rm/find/backend).
// Each takes a b2.Backend (and io for prompts/output) so they are testable.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/store"
)

// remoteKey normalizes a user-supplied path to a bucket key (forward slashes,
// no leading slash).
func remoteKey(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(p), "/")
}

// underBase reports whether dest is within base (prevents path traversal from
// server-supplied keys).
func underBase(base, dest string) bool {
	rel, err := filepath.Rel(base, dest)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Ls lists a prefix. Folders (common prefixes) are shown with a trailing slash.
func Ls(ctx context.Context, be b2.Backend, path string, recursive bool, out io.Writer) error {
	prefix := path
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	prefix = strings.TrimPrefix(filepath.ToSlash(prefix), "/")
	l, err := be.List(ctx, prefix, recursive)
	if err != nil {
		return err
	}
	for _, p := range l.Prefixes {
		fmt.Fprintf(out, "%s\n", p)
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	for _, o := range l.Objects {
		fmt.Fprintf(tw, "%s\t%d\n", o.Key, o.Size)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(l.Prefixes) == 0 && len(l.Objects) == 0 {
		fmt.Fprintln(out, "(empty)")
	}
	return nil
}

// Find prints keys (optionally under prefix) whose name contains query (case-insensitive).
func Find(ctx context.Context, be b2.Backend, query, prefix string, out io.Writer) error {
	prefix = strings.TrimPrefix(filepath.ToSlash(prefix), "/")
	l, err := be.List(ctx, prefix, true)
	if err != nil {
		return err
	}
	q := strings.ToLower(query)
	n := 0
	for _, o := range l.Objects {
		if strings.Contains(strings.ToLower(o.Key), q) {
			fmt.Fprintf(out, "%s\n", o.Key)
			n++
		}
	}
	fmt.Fprintf(out, "%d match(es)\n", n)
	return nil
}

// Get downloads a single object, or (with recursive) every object under a prefix.
func Get(ctx context.Context, be b2.Backend, remote, local string, recursive bool, out io.Writer) error {
	key := remoteKey(remote)
	if recursive {
		prefix := key
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		l, err := be.List(ctx, prefix, true)
		if err != nil {
			return err
		}
		base := local
		if base == "" {
			base = "."
		}
		for _, o := range l.Objects {
			rel := strings.TrimPrefix(o.Key, prefix)
			dest := filepath.Join(base, filepath.FromSlash(rel))
			if !underBase(base, dest) {
				return fmt.Errorf("%w: refusing to write outside %s: %s", apperr.ErrDownloadFailed, base, o.Key)
			}
			if err := downloadTo(ctx, be, o.Key, dest); err != nil {
				return err
			}
			fmt.Fprintf(out, "downloaded %s\n", o.Key)
		}
		return nil
	}
	dest := local
	if dest == "" {
		dest = filepath.Base(filepath.FromSlash(key))
	}
	if err := downloadTo(ctx, be, key, dest); err != nil {
		return err
	}
	fmt.Fprintf(out, "downloaded %s -> %s\n", key, dest)
	return nil
}

// downloadTo fetches key from the backend and writes it to dest, creating parent directories as needed.
func downloadTo(ctx context.Context, be b2.Backend, key, dest string) error {
	if dir := filepath.Dir(dest); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	rc, _, err := be.Download(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, rc)
	return err
}

// Put uploads a single file, or (with recursive) every file under a local
// directory. In recursive mode a failed file does NOT abort the batch: it is
// reported, counted, and the walk continues, ending with a summary and a
// non-zero result if any file failed. When skipExisting is set, files whose
// remote object already exists are skipped (by presence only — this does not
// detect locally-changed files; use it to resume an interrupted upload).
func Put(ctx context.Context, be b2.Backend, local, remote string, recursive, skipExisting bool, out io.Writer) error {
	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if !recursive {
			return fmt.Errorf("%s is a directory (use -r)", local)
		}
		prefix := remoteKey(remote)
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		var uploaded, skipped, failed int
		walkErr := filepath.WalkDir(local, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err // a filesystem walk error aborts; nil for directories
			}
			rel, _ := filepath.Rel(local, p)
			key := prefix + filepath.ToSlash(rel)
			if skipExisting {
				if ok, e := be.Exists(ctx, key); e == nil && ok {
					skipped++
					fmt.Fprintf(out, "skipped (exists) %s\n", key)
					return nil
				}
			}
			if err := uploadFrom(ctx, be, p, key); err != nil {
				failed++
				fmt.Fprintf(out, "FAILED %s: %v\n", key, err)
				return nil // keep going — don't abandon the rest of the batch
			}
			uploaded++
			fmt.Fprintf(out, "uploaded %s\n", key)
			return nil
		})
		if walkErr != nil {
			return walkErr
		}
		fmt.Fprintf(out, "Uploaded: %d, Skipped: %d, Failed: %d\n", uploaded, skipped, failed)
		if failed > 0 {
			return fmt.Errorf("%w: %d file(s) failed to upload", apperr.ErrUploadFailed, failed)
		}
		return nil
	}
	key := remoteKey(remote)
	if key == "" {
		key = filepath.Base(local)
	}
	if skipExisting {
		if ok, e := be.Exists(ctx, key); e == nil && ok {
			fmt.Fprintf(out, "skipped (exists) %s\n", key)
			return nil
		}
	}
	if err := uploadFrom(ctx, be, local, key); err != nil {
		return err
	}
	fmt.Fprintf(out, "uploaded %s -> %s\n", local, key)
	return nil
}

// uploadFrom opens the local file and uploads its contents to the backend under key.
func uploadFrom(ctx context.Context, be b2.Backend, local, key string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return be.Upload(ctx, key, f, info.Size())
}

// Rm deletes an object, or (with recursive) every object under a prefix. Prompts
// for confirmation unless force is set.
//
// bucket is the configured bucket name, used only in messages: every prompt,
// line and summary says the deletion happens in the bucket, so output listing
// home/... keys can't be mistaken for local files being removed. Rm never
// touches the local filesystem.
func Rm(ctx context.Context, be b2.Backend, bucket, path string, recursive, force bool, in io.Reader, out io.Writer) error {
	if recursive {
		prefix := remoteKey(path)
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		l, err := be.List(ctx, prefix, true)
		if err != nil {
			return err
		}
		if len(l.Objects) == 0 {
			fmt.Fprintf(out, "nothing to delete under %q in bucket %q\n", prefix, bucket)
			return nil
		}
		q := fmt.Sprintf("Delete %d object(s) under %q from bucket %q (local files are not affected)?",
			len(l.Objects), prefix, bucket)
		if !force && !confirm(in, out, q) {
			fmt.Fprintln(out, "aborted")
			return nil
		}
		for i, o := range l.Objects {
			if err := be.Delete(ctx, o.Key); err != nil {
				return fmt.Errorf("%w (deleted %d of %d object(s) from bucket %q before this error)",
					err, i, len(l.Objects), bucket)
			}
			fmt.Fprintf(out, "deleted (remote) %s\n", o.Key)
		}
		fmt.Fprintf(out, "Deleted %d object(s) from bucket %q. Local files were not touched.\n", len(l.Objects), bucket)
		return nil
	}
	key := remoteKey(path)
	if !force && !confirm(in, out, fmt.Sprintf("Delete %q from bucket %q (local files are not affected)?", key, bucket)) {
		fmt.Fprintln(out, "aborted")
		return nil
	}
	if err := be.Delete(ctx, key); err != nil {
		return err
	}
	fmt.Fprintf(out, "deleted (remote) %s from bucket %q. Local files were not touched.\n", key, bucket)
	return nil
}

// confirm prints prompt and returns true only if the user answers yes.
func confirm(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprintf(out, "%s [y/N]: ", prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

// Buckets lists every bucket visible to the stored credentials, marking the one
// currently selected in config with "*". It always queries the native B2 API
// (via b2.ListBuckets) since only that API returns bucket IDs.
//
// listAll is b2.ListBuckets and findOne is b2.FindBucket (injected for tests).
// A key restricted to one bucket is refused an unfiltered listing, so on an
// access-denied error Buckets falls back to looking up the configured bucket
// by name and says why only that one is shown.
func Buckets(ctx context.Context, cfg b2.Config, listAll, findOne BucketLister, out io.Writer) error {
	list, err := listAll(ctx, cfg)
	if err != nil {
		if !errors.Is(err, apperr.ErrAccessDenied) || cfg.BucketName == "" {
			return err
		}
		one, ferr := findOne(ctx, cfg)
		if ferr != nil {
			return err // report the original denial; the fallback added nothing
		}
		fmt.Fprintln(out, "This key cannot list every bucket (it is restricted to one); showing the configured bucket only.")
		list = one
	}
	if len(list) == 0 {
		fmt.Fprintln(out, "(no buckets)")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "\tNAME\tID\tTYPE")
	for _, bk := range list {
		marker := " "
		if strings.EqualFold(bk.Name, cfg.BucketName) { // B2 names are not case sensitive
			marker = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", marker, bk.Name, bk.ID, bk.Type)
	}
	return tw.Flush()
}

// Backend prints (no arg) or sets the stored backend mode.
func Backend(ctx context.Context, st *store.Store, kind string, out io.Writer) error {
	if kind == "" {
		cur, err := st.GetBackend(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Backend: %s\n", cur)
		return nil
	}
	if err := st.SetBackend(ctx, kind); err != nil {
		return err
	}
	fmt.Fprintf(out, "Backend set to %s\n", kind)
	return nil
}
