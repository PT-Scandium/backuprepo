// CLI handlers for manual bucket operations (ls/get/put/rm/find/backend).
// Each takes a b2.Backend (and io for prompts/output) so they are testable.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"backuprepo/internal/b2"
	"backuprepo/internal/store"
)

// remoteKey normalizes a user-supplied path to a bucket key (forward slashes,
// no leading slash).
func remoteKey(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(p), "/")
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
	tw.Flush()
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

// Put uploads a single file, or (with recursive) every file under a local directory.
func Put(ctx context.Context, be b2.Backend, local, remote string, recursive bool, out io.Writer) error {
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
		return filepath.WalkDir(local, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(local, p)
			key := prefix + filepath.ToSlash(rel)
			if err := uploadFrom(ctx, be, p, key); err != nil {
				return err
			}
			fmt.Fprintf(out, "uploaded %s\n", key)
			return nil
		})
	}
	key := remoteKey(remote)
	if key == "" {
		key = filepath.Base(local)
	}
	if err := uploadFrom(ctx, be, local, key); err != nil {
		return err
	}
	fmt.Fprintf(out, "uploaded %s -> %s\n", local, key)
	return nil
}

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
func Rm(ctx context.Context, be b2.Backend, path string, recursive, force bool, in io.Reader, out io.Writer) error {
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
			fmt.Fprintln(out, "nothing to delete")
			return nil
		}
		if !force && !confirm(in, out, fmt.Sprintf("Delete %d object(s) under %q?", len(l.Objects), prefix)) {
			fmt.Fprintln(out, "aborted")
			return nil
		}
		for _, o := range l.Objects {
			if err := be.Delete(ctx, o.Key); err != nil {
				return err
			}
			fmt.Fprintf(out, "deleted %s\n", o.Key)
		}
		return nil
	}
	key := remoteKey(path)
	if !force && !confirm(in, out, fmt.Sprintf("Delete %q?", key)) {
		fmt.Fprintln(out, "aborted")
		return nil
	}
	if err := be.Delete(ctx, key); err != nil {
		return err
	}
	fmt.Fprintf(out, "deleted %s\n", key)
	return nil
}

func confirm(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprintf(out, "%s [y/N]: ", prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
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
