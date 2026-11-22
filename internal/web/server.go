// Package web serves backuprepo's localhost management UI (bb serve, port 9171).
// It is intentionally unauthenticated per the spec, so it binds to 127.0.0.1
// only and rejects non-localhost Host headers (DNS-rebinding guard). Browsing
// and deletion are confined to the configured watched folders — a no-auth server
// must never expose or delete arbitrary filesystem paths.
package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/backup"
	"backuprepo/internal/store"
)

// Addr is the localhost address the web UI binds to.
const Addr = "127.0.0.1:9171"

// Server is the localhost web UI.
type Server struct {
	store    *store.Store
	be       b2.Backend
	location string // server location shown in the header
	username string // OS login user shown in the header
	done     chan struct{}
	once     sync.Once
	syncFn   func(ctx context.Context) error // optional: sync mapped folders after Upload
}

// New builds a Server. location is a human label for the destination (bucket /
// endpoint) shown in the page header.
func New(st *store.Store, be b2.Backend, location string) *Server {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return &Server{store: st, be: be, location: location, username: name, done: make(chan struct{})}
}

// SetSync makes the Upload button also sync the mapped folders (ADR-028).
func (s *Server) SetSync(fn func(ctx context.Context) error) { s.syncFn = fn }

// Serve runs the UI until Ctrl-C, the Close button, or ctx cancellation, then
// shuts down gracefully.
func (s *Server) Serve(ctx context.Context, out io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	srv := &http.Server{Addr: Addr, Handler: s.routes()}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Fprintf(out, "Serving the bb web UI at http://%s  (Ctrl-C or the Close button to stop)\n", Addr)

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("%w: serve: %v", apperr.ErrDaemon, err)
		}
		return nil
	case <-ctx.Done():
	case <-s.done:
	}
	fmt.Fprintln(out, "Shutting down web UI.")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// routes builds the mux, wrapping handlers with the localhost guard and (for
// state-changing endpoints) the POST/CSRF check.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.guard(s.handleIndex))
	mux.HandleFunc("/upload", s.guard(s.requirePost(s.handleUpload)))
	mux.HandleFunc("/delete", s.guard(s.requirePost(s.handleDelete)))
	mux.HandleFunc("/close", s.guard(s.requirePost(s.handleClose)))
	return mux
}

// guard rejects any request whose Host header isn't localhost, defeating
// DNS-rebinding attacks against this unauthenticated local server.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden: localhost only", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// requirePost rejects non-POST methods and cross-origin requests before invoking h.
func (s *Server) requirePost(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !sameOrigin(r) {
			http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// sameOrigin is the CSRF defense for the state-changing POST endpoints. The
// Host-header guard stops DNS-rebinding but NOT a cross-site form POST (the
// browser sends those with the real Host), so we additionally require the
// request's Origin (or Referer, as a fallback) to match this server's host. A
// cross-site attacker's page carries its own Origin and is rejected; the UI's
// own forms carry ours. Requests with neither header are rejected too — the
// UI's forms always send Origin, so only non-browser clients lack it, and those
// should use the `bb` CLI rather than these endpoints.
func sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// handleIndex renders the watched-folder list at the root and the directory
// listing when drilling into a watched path.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := r.URL.Query().Get("path")

	data := pageData{Username: s.username, Location: s.location}

	if path == "" {
		// Root: list the watched folders as drill-in entries.
		folders, err := s.store.ListFolders(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.Crumbs = []crumb{{Name: "Home", Href: "/"}}
		data.AtRoot = true
		for _, f := range folders {
			data.Rows = append(data.Rows, row{
				Name: f, Href: "/?path=" + url.QueryEscape(f), IsDir: true,
				Type: "folder", Size: "—", Owner: "—", LastBackup: "—",
			})
		}
		if len(folders) == 0 {
			data.Message = "No folders are being watched. Add one with `bb watch <dir>`."
		}
		render(w, data)
		return
	}

	root, ok := s.resolveWatched(ctx, path)
	if !ok {
		http.Error(w, "forbidden: path is not within a watched folder", http.StatusForbidden)
		return
	}
	clean := filepath.Clean(path)
	data.Path = clean
	data.Crumbs = s.breadcrumbs(root, clean)

	entries, err := os.ReadDir(clean)
	if err != nil {
		data.Message = "Cannot read directory: " + err.Error()
		render(w, data)
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir() // directories first
		}
		return entries[i].Name() < entries[j].Name()
	})
	for _, e := range entries {
		full := filepath.Join(clean, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		data.Rows = append(data.Rows, s.buildRow(ctx, full, info))
	}
	render(w, data)
}

// buildRow assembles one table row from a filesystem entry plus its stored
// backup state.
func (s *Server) buildRow(ctx context.Context, full string, info os.FileInfo) row {
	r := row{Name: info.Name(), Path: full, Owner: fileOwner(info)}
	if info.IsDir() {
		r.IsDir = true
		r.Type = "folder"
		r.Size = "—"
		r.LastBackup = "—"
		r.Href = "/?path=" + url.QueryEscape(full)
	} else {
		ext := strings.TrimPrefix(filepath.Ext(info.Name()), ".")
		if ext == "" {
			ext = "file"
		}
		r.Type = ext
		r.Size = humanSize(info.Size())
		r.LastBackup = "never"
		if rec, err := s.store.GetFile(ctx, full); err == nil && rec != nil && rec.LastBackup != nil {
			r.LastBackup = time.Unix(*rec.LastBackup, 0).Format("2006-01-02 15:04")
		}
	}
	r.Modified = info.ModTime().Format("2006-01-02 15:04")
	return r
}

// handleUpload backs up all changed files, then redirects back to the current page.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	// A per-file failure is reflected in the refreshed table (Last Backup
	// unchanged); don't 500 the whole page over it.
	_, _ = backup.New(s.store, s.be).UploadChanged(r.Context())
	if s.syncFn != nil {
		// Mapped folders (ADR-028): the report goes to the `bb serve` terminal;
		// a failure is logged there too, and the table shows backup times.
		if err := s.syncFn(r.Context()); err != nil {
			log.Printf("sync: %v", err)
		}
	}
	http.Redirect(w, r, redirectTarget(r), http.StatusSeeOther)
}

// handleDelete deletes a watched path locally and remotely, then redirects to its
// parent (or root).
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := r.FormValue("path")
	if _, ok := s.resolveWatched(ctx, path); !ok {
		http.Error(w, "forbidden: path is not within a watched folder", http.StatusForbidden)
		return
	}
	if err := s.deletePath(ctx, filepath.Clean(path)); err != nil {
		http.Error(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Return to the parent directory if it is still within a watched folder.
	parent := filepath.Dir(filepath.Clean(path))
	target := "/"
	if _, ok := s.resolveWatched(ctx, parent); ok {
		target = "/?path=" + url.QueryEscape(parent)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// deletePath permanently removes path locally AND its backup in the bucket. For
// a directory it purges the remote object of every tracked file beneath it, then
// removes the local tree.
func (s *Server) deletePath(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	files, err := s.store.ListFiles(ctx)
	if err != nil {
		return err
	}
	// Match records the way the OS matches paths: resolveWatched (filepath.Rel)
	// accepts a differently-cased path on Windows, so an exact comparison here
	// would delete the local files but leave their backups and records behind.
	recs := recordsToDelete(files, path, info.IsDir(), runtime.GOOS == "windows", string(filepath.Separator))
	if info.IsDir() {
		for _, fr := range recs {
			if derr := s.be.Delete(ctx, fr.RemoteKey); derr != nil && !errors.Is(derr, apperr.ErrObjectNotFound) {
				return derr
			}
			_ = s.store.RemoveFile(ctx, fr.Path)
		}
		return os.RemoveAll(path)
	}
	// Delete the remote object first so a failure leaves the local file intact.
	// Use the stored record's key and path when there is one: they carry the
	// casing the file was backed up under.
	folders, err := s.store.ListWatched(ctx)
	if err != nil {
		return err
	}
	key, recPath := backup.KeyFor(folders, path), path
	if len(recs) > 0 {
		key, recPath = recs[0].RemoteKey, recs[0].Path
	}
	if err := s.be.Delete(ctx, key); err != nil && !errors.Is(err, apperr.ErrObjectNotFound) {
		return err
	}
	_ = s.store.RemoveFile(ctx, recPath)
	return os.Remove(path)
}

// recordsToDelete returns the records whose backups a delete of path should
// purge: the record equal to path, or for a directory every record at or
// beneath it. With fold (Windows) names compare case-insensitively, as the OS
// does, but a record that matches only by folding must also name the same
// directory or file on disk (os.SameFile): a per-directory case-sensitive
// folder can hold both Docs and docs, and deleting Docs must not purge the
// backups of docs. A record whose path can't be checked is left alone (its
// backup is kept rather than deleted).
func recordsToDelete(files []store.FileRecord, path string, isDir, fold bool, sep string) []store.FileRecord {
	root := strings.TrimSuffix(path, sep)
	var rootInfo os.FileInfo
	var out []store.FileRecord
	for _, fr := range trackedRecords(files, path, isDir, fold, sep) {
		head := fr.Path // the part of fr.Path that names path itself
		if len(fr.Path) > len(root) {
			head = fr.Path[:len(root)]
		}
		if head == root {
			out = append(out, fr)
			continue
		}
		if rootInfo == nil {
			var err error
			if rootInfo, err = os.Stat(root); err != nil {
				return out
			}
		}
		if hi, err := os.Stat(head); err == nil && os.SameFile(hi, rootInfo) {
			out = append(out, fr)
		}
	}
	return out
}

// trackedRecords returns the records for path by name alone: the one equal to
// it, or for a directory every record at or beneath it, comparing
// case-insensitively when fold is set. sep is the path separator.
func trackedRecords(files []store.FileRecord, path string, isDir, fold bool, sep string) []store.FileRecord {
	var out []store.FileRecord
	for _, fr := range files {
		if pathEqual(fr.Path, path, fold) || isDir && pathUnder(fr.Path, path, fold, sep) {
			out = append(out, fr)
		}
	}
	return out
}

// pathEqual compares two cleaned paths, case-insensitively when fold is set
// (Windows: NTFS default).
func pathEqual(a, b string, fold bool) bool {
	if fold {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// pathUnder reports whether p lies strictly beneath dir.
func pathUnder(p, dir string, fold bool, sep string) bool {
	prefix := strings.TrimSuffix(dir, sep) + sep
	return len(p) > len(prefix) && pathEqual(p[:len(prefix)], prefix, fold)
}

// handleClose renders the closed page and signals Serve to shut the server down.
func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	renderClosed(w)
	s.once.Do(func() { close(s.done) })
}

// resolveWatched reports the watched folder that contains (or equals) path, and
// whether path is allowed at all. Purely lexical — confines browsing/deletion to
// configured folders.
func (s *Server) resolveWatched(ctx context.Context, path string) (string, bool) {
	folders, err := s.store.ListFolders(ctx)
	if err != nil {
		return "", false
	}
	clean := filepath.Clean(path)
	for _, f := range folders {
		f = filepath.Clean(f)
		if clean == f {
			return f, true
		}
		if rel, err := filepath.Rel(f, clean); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return f, true
		}
	}
	return "", false
}

// breadcrumbs builds the Home → root → ... → path navigation trail for path.
func (s *Server) breadcrumbs(root, path string) []crumb {
	cr := []crumb{
		{Name: "Home", Href: "/"},
		{Name: filepath.Base(root), Href: "/?path=" + url.QueryEscape(root)},
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == "" {
		return cr
	}
	acc := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		acc = filepath.Join(acc, part)
		cr = append(cr, crumb{Name: part, Href: "/?path=" + url.QueryEscape(acc)})
	}
	return cr
}

// redirectTarget returns the current path (from a form field) so an action
// returns the user to where they were; falls back to root.
func redirectTarget(r *http.Request) string {
	if p := r.FormValue("path"); p != "" {
		return "/?path=" + url.QueryEscape(p)
	}
	return "/"
}

// humanSize formats a byte count as a human-readable string (B, KiB, MiB, ...).
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
