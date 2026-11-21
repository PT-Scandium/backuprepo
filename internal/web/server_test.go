package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"backuprepo/internal/b2"
	"backuprepo/internal/backup"
	"backuprepo/internal/store"
)

// key32 returns a deterministic 32-byte test key.
func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// newTestServer returns a Server backed by a temp store + fake bucket, watching
// a temp dir, and the dir path.
func newTestServer(t *testing.T) (*Server, *store.Store, *b2.FakeBackend, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "w.db"), key32())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	if err := st.AddFolder(ctx, dir); err != nil {
		t.Fatal(err)
	}
	fake := b2.NewFake()
	return New(st, fake, "test-bucket"), st, fake, dir
}

// get issues a localhost GET against the server's mux.
func do(s *Server, method, target string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Host = "127.0.0.1:9171" // satisfy the DNS-rebinding guard
	if method == http.MethodPost {
		r.Header.Set("Origin", "http://127.0.0.1:9171") // satisfy the CSRF check
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, r)
	return rec
}

// TestIndexListsWatchedFolderAndFiles verifies the root lists the watched folder
// and drilling in shows its files with a "never" backup status.
func TestIndexListsWatchedFolderAndFiles(t *testing.T) {
	s, _, _, dir := newTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Root lists the watched folder.
	root := do(s, "GET", "/", "")
	if root.Code != 200 || !strings.Contains(root.Body.String(), dir) {
		t.Fatalf("root listing missing watched folder: code=%d", root.Code)
	}

	// Drilling into it shows the file with a "never" backup status.
	page := do(s, "GET", "/?path="+url.QueryEscape(dir), "")
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "report.txt") || !strings.Contains(body, "never") {
		t.Fatalf("folder listing missing file/status: code=%d body=%q", page.Code, body)
	}
}

// TestPathTraversalRejected verifies a path outside any watched folder is rejected with 403.
func TestPathTraversalRejected(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	rec := do(s, "GET", "/?path="+url.QueryEscape("/etc"), "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for path outside watched folders, got %d", rec.Code)
	}
}

// TestHostGuardRejectsNonLocalhost verifies a non-localhost Host header is rejected with 403.
func TestHostGuardRejectsNonLocalhost(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.example.com"
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-localhost Host, got %d", rec.Code)
	}
}

// TestCSRFRejectsCrossOriginPost verifies POSTs with a cross-origin Origin or no
// Origin/Referer are rejected with 403.
func TestCSRFRejectsCrossOriginPost(t *testing.T) {
	s, _, _, _ := newTestServer(t)
	// A cross-site form POST carries the attacker's Origin and must be rejected.
	cross := httptest.NewRequest("POST", "/close", nil)
	cross.Host = "127.0.0.1:9171"
	cross.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, cross)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST should be 403, got %d", rec.Code)
	}

	// A POST with no Origin/Referer is also rejected (fail closed).
	none := httptest.NewRequest("POST", "/close", nil)
	none.Host = "127.0.0.1:9171"
	rec2 := httptest.NewRecorder()
	s.routes().ServeHTTP(rec2, none)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("origin-less POST should be 403, got %d", rec2.Code)
	}
}

// TestUploadButtonBacksUp verifies the upload endpoint backs up a changed file
// and redirects (303).
func TestUploadButtonBacksUp(t *testing.T) {
	s, _, fake, dir := newTestServer(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := do(s, "POST", "/upload", "path="+url.QueryEscape(dir))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}
	if len(fake.Objects) != 1 {
		t.Fatalf("expected 1 uploaded object, got %d", len(fake.Objects))
	}
}

// TestDeleteRemovesLocalAndRemote verifies the delete endpoint removes the local
// file, the remote object, and the DB record.
func TestDeleteRemovesLocalAndRemote(t *testing.T) {
	s, st, fake, dir := newTestServer(t)
	ctx := context.Background()
	file := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(file, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Back it up first so there's a remote object + DB record to remove.
	if _, err := backup.New(st, fake).UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Objects[backup.RemoteKey(file)]; !ok {
		t.Fatal("precondition: file should be backed up")
	}

	rec := do(s, "POST", "/delete", "path="+url.QueryEscape(file))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d (%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("local file should be deleted, stat err = %v", err)
	}
	if _, ok := fake.Objects[backup.RemoteKey(file)]; ok {
		t.Fatal("remote object should be deleted")
	}
	if rec, err := st.GetFile(ctx, file); err != nil || rec != nil {
		t.Fatalf("file record should be removed, got rec=%v err=%v", rec, err)
	}
}

// TestTrackedRecordsFoldsCaseOnWindows checks record matching for the web
// delete: case-insensitive on Windows (as resolveWatched's filepath.Rel is),
// exact elsewhere, and directory matches stay on component boundaries.
func TestTrackedRecordsFoldsCaseOnWindows(t *testing.T) {
	win := []store.FileRecord{
		{Path: `C:\Users\X\Docs\a.txt`, RemoteKey: "ka"},
		{Path: `C:\Users\X\Docs\sub\b.txt`, RemoteKey: "kb"},
		{Path: `C:\Users\X\Docsextra\c.txt`, RemoteKey: "kc"},
	}
	keys := func(rs []store.FileRecord) string {
		var ks []string
		for _, r := range rs {
			ks = append(ks, r.RemoteKey)
		}
		return strings.Join(ks, ",")
	}
	for _, tc := range []struct {
		files []store.FileRecord
		path  string
		isDir bool
		goos  string
		want  string
	}{
		{win, `c:\users\x\docs`, true, "windows", "ka,kb"}, // not Docsextra
		{win, `c:\users\x\docs\A.TXT`, false, "windows", "ka"},
		{win, `C:\Users\X\Docs\`, true, "windows", "ka,kb"}, // trailing separator
		{win, `c:\users\x\docs\a.txt`, false, "linux", ""},
		{[]store.FileRecord{{Path: "/home/u/Docs/a", RemoteKey: "k1"}, {Path: "/home/u/docs/b", RemoteKey: "k2"}},
			"/home/u/Docs", true, "linux", "k1"},
	} {
		fold, sep := tc.goos == "windows", "/"
		if fold {
			sep = `\`
		}
		if got := keys(trackedRecords(tc.files, tc.path, tc.isDir, fold, sep)); got != tc.want {
			t.Errorf("trackedRecords(%q, dir=%v, %s) = %q, want %q", tc.path, tc.isDir, tc.goos, got, tc.want)
		}
	}
}

// TestRecordsToDeleteSparesCaseSensitiveSibling is the guard for folding on a
// per-directory case-sensitive Windows folder, where Docs and docs are two
// directories: deleting Docs must not purge the backups under docs. Linux's
// case-sensitive filesystem stands in for such a folder (fold on, "/").
func TestRecordsToDeleteSparesCaseSensitiveSibling(t *testing.T) {
	base := t.TempDir()
	upper, lower := filepath.Join(base, "Docs"), filepath.Join(base, "docs")
	if err := os.Mkdir(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	// On a case-insensitive filesystem (default Windows NTFS, macOS APFS)
	// "docs" already exists as Docs, so there is no sibling to protect.
	if err := os.Mkdir(lower, 0o755); errors.Is(err, os.ErrExist) {
		t.Skip("filesystem is case-insensitive; Docs and docs are one directory")
	} else if err != nil {
		t.Fatal(err)
	}
	files := []store.FileRecord{
		{Path: filepath.Join(upper, "a.txt"), RemoteKey: "upper-a"},
		{Path: filepath.Join(lower, "b.txt"), RemoteKey: "lower-b"},
	}
	sep := string(filepath.Separator)
	var got []string
	for _, r := range recordsToDelete(files, upper, true, true, sep) {
		got = append(got, r.RemoteKey)
	}
	if strings.Join(got, ",") != "upper-a" {
		t.Errorf("deleting Docs purges %v, want only [upper-a]", got)
	}
	// A case-variant spelling of the same directory still matches: delete
	// "DOCS" when only Docs exists on disk.
	if err := os.Remove(lower); err != nil {
		t.Fatal(err)
	}
	if r := recordsToDelete(files[:1], filepath.Join(base, "DOCS"), true, true, sep); len(r) != 0 {
		// On a case-sensitive filesystem "DOCS" does not exist, so the
		// fold-only match cannot be confirmed and the backup is kept.
		t.Errorf("unconfirmable fold match should be kept, got %v", r)
	}
}

// TestDeleteDifferentCaseRemovesBackupOnWindows is the regression for the web
// delete on Windows: a differently-cased path passed resolveWatched, deleted
// the local file, and left its remote object and record behind.
func TestDeleteDifferentCaseRemovesBackupOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("needs a case-insensitive filesystem (Windows)")
	}
	s, st, fake, dir := newTestServer(t)
	ctx := context.Background()
	top := filepath.Join(dir, "Report.txt")
	sub := filepath.Join(dir, "Sub")
	inSub := filepath.Join(sub, "Inner.txt")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{top, inSub} {
		if err := os.WriteFile(f, []byte("bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := backup.New(st, fake).UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ target, file string }{
		{strings.ToUpper(top), top},   // a file, upper-cased
		{strings.ToLower(sub), inSub}, // a directory, lower-cased
	} {
		rec := do(s, "POST", "/delete", "path="+url.QueryEscape(tc.target))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("delete %s: got %d (%s)", tc.target, rec.Code, rec.Body.String())
		}
		if _, err := os.Stat(tc.file); !os.IsNotExist(err) {
			t.Errorf("delete %s: local file still present (stat err %v)", tc.target, err)
		}
		if _, ok := fake.Objects[backup.RemoteKey(tc.file)]; ok {
			t.Errorf("delete %s: remote object still present", tc.target)
		}
		if r, err := st.GetFile(ctx, tc.file); err != nil || r != nil {
			t.Errorf("delete %s: record still present (rec=%v err=%v)", tc.target, r, err)
		}
	}
}
