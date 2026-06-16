package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
