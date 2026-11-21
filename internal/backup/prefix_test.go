package backup

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"backuprepo/internal/b2"
	"backuprepo/internal/store"
)

// sha1Hex returns the hex SHA-1 of s, as B2 reports contentSha1.
func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// mkdirAll creates dir (and parents), failing the test on error.
func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestKeyFor covers the folder → object-key mapping (ADR-027): the innermost
// watched folder decides; a prefix replaces the folder's own path; no prefix
// (or no containing folder) keeps the default absolute-path key (ADR-003).
func TestKeyFor(t *testing.T) {
	root := t.TempDir()
	docs := filepath.Join(root, "Documents")
	sewa := filepath.Join(docs, "SEWA")
	file := filepath.Join(sewa, "2025", "a.pdf")
	notes := filepath.Join(docs, "notes.md")
	for _, tc := range []struct {
		name    string
		folders []store.Folder
		path    string
		want    string
	}{
		{"no folders", nil, file, RemoteKey(file)},
		{"folder without prefix", []store.Folder{{Path: docs}}, file, RemoteKey(file)},
		{"prefixed folder", []store.Folder{{Path: sewa, RemotePrefix: "SEWA/"}}, file, "SEWA/2025/a.pdf"},
		{"trailing separator on folder", []store.Folder{{Path: sewa + string(filepath.Separator), RemotePrefix: "P/"}}, file, "P/2025/a.pdf"},
		{"inner prefixed wins over outer plain",
			[]store.Folder{{Path: docs}, {Path: sewa, RemotePrefix: "SEWA/"}}, file, "SEWA/2025/a.pdf"},
		{"outer plain still owns its other files",
			[]store.Folder{{Path: docs}, {Path: sewa, RemotePrefix: "SEWA/"}}, notes, RemoteKey(notes)},
		{"inner plain wins over outer prefixed",
			[]store.Folder{{Path: docs, RemotePrefix: "DOCS/"}, {Path: sewa}}, file, RemoteKey(file)},
		{"order of folders does not matter",
			[]store.Folder{{Path: sewa, RemotePrefix: "SEWA/"}, {Path: docs, RemotePrefix: "DOCS/"}}, file, "SEWA/2025/a.pdf"},
		{"sibling sharing a name prefix is not a parent",
			[]store.Folder{{Path: sewa + "X", RemotePrefix: "X/"}}, file, RemoteKey(file)},
		{"path equal to the folder", []store.Folder{{Path: sewa, RemotePrefix: "SEWA/"}}, sewa, RemoteKey(sewa)},
	} {
		if got := KeyFor(tc.folders, tc.path); got != tc.want {
			t.Errorf("%s: KeyFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestUploadChangedUsesFolderPrefix: files in a prefixed folder are uploaded
// under prefix + relative path, and the record stores that key.
func TestUploadChangedUsesFolderPrefix(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "2025"))
	writeFile(t, filepath.Join(dir, "2025", "a.pdf"), "pdf")
	if err := st.AddFolderWithPrefix(ctx, dir, "SEWA/"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Uploaded != 1 {
		t.Fatalf("UploadChanged = %+v, %v; want 1 upload", res, err)
	}
	if string(fake.Objects["SEWA/2025/a.pdf"]) != "pdf" || len(fake.Objects) != 1 {
		t.Fatalf("objects = %v, want only SEWA/2025/a.pdf", keys(fake.Objects))
	}
	if rec, _ := st.GetFile(ctx, filepath.Join(dir, "2025", "a.pdf")); rec == nil || rec.RemoteKey != "SEWA/2025/a.pdf" {
		t.Errorf("record = %+v, want RemoteKey SEWA/2025/a.pdf", rec)
	}
}

// TestNestedWatchedFoldersUploadEachFileOnce: with ~/Documents (no prefix) and
// ~/Documents/SEWA (prefix) both watched, each file is uploaded exactly once
// under its innermost folder's key, and a second run uploads nothing (no
// flip-flopping between the two keys).
func TestNestedWatchedFoldersUploadEachFileOnce(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	docs := t.TempDir()
	sewa := filepath.Join(docs, "SEWA")
	mkdirAll(t, sewa)
	writeFile(t, filepath.Join(docs, "notes.md"), "n")
	writeFile(t, filepath.Join(sewa, "a.pdf"), "a")
	if err := st.AddFolder(ctx, docs); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolderWithPrefix(ctx, sewa, "SEWA/"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Uploaded != 2 || res.Skipped != 0 {
		t.Fatalf("first run = %+v, %v; want 2 uploads, 0 skipped (one visit per file)", res, err)
	}
	want := map[string]bool{"SEWA/a.pdf": true, RemoteKey(filepath.Join(docs, "notes.md")): true}
	if len(fake.Objects) != 2 {
		t.Fatalf("objects = %v, want %v", keys(fake.Objects), want)
	}
	for k := range want {
		if _, ok := fake.Objects[k]; !ok {
			t.Errorf("missing object %q (have %v)", k, keys(fake.Objects))
		}
	}
	res, err = svc.UploadChanged(ctx)
	if err != nil || res.Uploaded != 0 || res.Skipped != 2 {
		t.Fatalf("second run = %+v, %v; want 0 uploads, 2 skipped", res, err)
	}
	if n, err := svc.PendingCount(ctx); err != nil || n != 0 {
		t.Errorf("PendingCount = %d, %v; want 0", n, err)
	}
}

// TestPrefixChangeReuploadsUnderNewKey: once a folder's prefix changes, its
// already backed-up files are uploaded again under the new key, then settle.
func TestPrefixChangeReuploadsUnderNewKey(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	writeFile(t, file, "x")
	if err := st.AddFolder(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFolderPrefix(ctx, dir, "NEW/"); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.PendingCount(ctx); n != 1 {
		t.Errorf("PendingCount after prefix change = %d, want 1", n)
	}
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Uploaded != 1 {
		t.Fatalf("after prefix change = %+v, %v; want 1 upload", res, err)
	}
	if _, ok := fake.Objects["NEW/a.txt"]; !ok {
		t.Fatalf("objects = %v, want NEW/a.txt", keys(fake.Objects))
	}
	if _, ok := fake.Objects[RemoteKey(file)]; !ok {
		t.Error("the object under the old name must be kept")
	}
	if res, _ := svc.UploadChanged(ctx); res.Uploaded != 0 {
		t.Errorf("third run uploaded %d, want 0", res.Uploaded)
	}
}

// TestUpgradeKeepsExistingBackups: records written before ADR-027 (key = the
// default name, or no key at all) are not uploaded again after upgrading.
func TestUpgradeKeepsExistingBackups(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := st.AddFolder(ctx, dir); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, key string }{{"keyed.txt", ""}, {"unkeyed.txt", "-"}} {
		path := filepath.Join(dir, tc.name)
		writeFile(t, path, tc.name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		key := RemoteKey(path)
		if tc.key == "-" {
			key = "" // a record from before remote keys were stored
		}
		ts := int64(1)
		if err := st.UpsertFile(ctx, store.FileRecord{Path: path, Size: info.Size(), ModTime: info.ModTime().Unix(),
			SHA256: "old", LastBackup: &ts, RemoteKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Uploaded != 0 || res.Skipped != 2 {
		t.Fatalf("UploadChanged = %+v, %v; want 0 uploads, 2 skipped", res, err)
	}
	if len(fake.Objects) != 0 {
		t.Errorf("uploaded %v, want nothing", keys(fake.Objects))
	}
}

// TestAdoptIdenticalRemoteObject: a new file whose exact content is already in
// the bucket under its key (same SHA-1) is recorded without an upload — e.g.
// after `bb get -r` into a prefixed folder — and stays settled afterwards.
func TestAdoptIdenticalRemoteObject(t *testing.T) {
	svc, st, fake := newSvc(t)
	fake.ReportSHA1 = true
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.pdf")
	writeFile(t, file, "contract")
	fake.Objects["SEWA/a.pdf"] = []byte("contract")
	if err := st.AddFolderWithPrefix(ctx, dir, "SEWA/"); err != nil {
		t.Fatal(err)
	}
	fake.FailUpload = func(key string) error { return errors.New("must not upload " + key) }
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Adopted != 1 || res.Uploaded != 0 {
		t.Fatalf("UploadChanged = %+v, %v; want 1 adopted, 0 uploaded", res, err)
	}
	rec, _ := st.GetFile(ctx, file)
	if rec == nil || rec.LastBackup == nil || rec.RemoteKey != "SEWA/a.pdf" {
		t.Fatalf("record = %+v, want backed up under SEWA/a.pdf", rec)
	}
	if res, err := svc.UploadChanged(ctx); err != nil || res.Skipped != 1 || res.Adopted != 0 {
		t.Errorf("second run = %+v, %v; want 1 skipped", res, err)
	}
}

// TestAdoptFallsBackToUpload: whenever adopting can't prove the bucket holds
// the same bytes, the file is uploaded — different content, a hash B2 can't
// compare ("none" for large files), a backend without the capability, or a
// failed hash lookup.
func TestAdoptFallsBackToUpload(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *b2.FakeBackend)
		up    func(f *b2.FakeBackend) b2.Uploader
	}{
		{"different content", func(f *b2.FakeBackend) { f.ReportSHA1 = true; f.Objects["P/a.txt"] = []byte("old") }, nil},
		{"large-file sha none", func(f *b2.FakeBackend) {
			f.ReportSHA1 = true
			f.Objects["P/a.txt"] = []byte("new")
			f.SHA1Override = map[string]string{"P/a.txt": "none"}
		}, nil},
		{"backend without hashes", func(f *b2.FakeBackend) { f.Objects["P/a.txt"] = []byte("new") }, nil},
		{"hash lookup fails", func(f *b2.FakeBackend) { f.Objects["P/a.txt"] = []byte("new") },
			func(f *b2.FakeBackend) b2.Uploader { return failingHasher{f} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st, fake := newSvc(t)
			tc.setup(fake)
			var up b2.Uploader = fake
			if tc.up != nil {
				up = tc.up(fake)
			}
			svc := New(st, up)
			ctx := context.Background()
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "a.txt"), "new")
			if err := st.AddFolderWithPrefix(ctx, dir, "P/"); err != nil {
				t.Fatal(err)
			}
			res, err := svc.UploadChanged(ctx)
			if err != nil || res.Uploaded != 1 || res.Adopted != 0 {
				t.Fatalf("UploadChanged = %+v, %v; want 1 upload, 0 adopted", res, err)
			}
			if wantFail := tc.name == "hash lookup fails"; (res.HashLookupsFailed == 1) != wantFail {
				t.Errorf("HashLookupsFailed = %d, want %v", res.HashLookupsFailed, wantFail)
			}
			if string(fake.Objects["P/a.txt"]) != "new" {
				t.Errorf("remote = %q, want the new content", fake.Objects["P/a.txt"])
			}
		})
	}
}

// TestChangedFileIsNotAdopted: an edit to a file already backed up under its
// key is always uploaded; the hash shortcut is only for files new to that key.
func TestChangedFileIsNotAdopted(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	writeFile(t, file, "v1")
	if err := st.AddFolderWithPrefix(ctx, dir, "P/"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}
	writeFile(t, file, "v2-longer")
	// Even if the server claimed to hold the new bytes, an edit must upload.
	fake.ReportSHA1 = true
	fake.SHA1Override = map[string]string{"P/a.txt": sha1Hex("v2-longer")}
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Uploaded != 1 || res.Adopted != 0 {
		t.Fatalf("UploadChanged = %+v, %v; want 1 upload", res, err)
	}
}

// failingHasher is an Uploader whose RemoteSHA1 always fails.
type failingHasher struct{ *b2.FakeBackend }

func (failingHasher) RemoteSHA1(context.Context, string) (string, bool, error) {
	return "", false, errors.New("b2 unavailable")
}

// keys lists a map's keys for failure messages.
func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestUnkeyedRecordMatchesOnlyDefaultName: a record from before keys were
// stored was backed up under the default name, so once its folder gets a
// prefix the file must be uploaded under the prefixed name (not silently
// treated as already there).
func TestUnkeyedRecordMatchesOnlyDefaultName(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	writeFile(t, path, "x")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := int64(1)
	if err := st.UpsertFile(ctx, store.FileRecord{Path: path, Size: info.Size(), ModTime: info.ModTime().Unix(),
		SHA256: "old", LastBackup: &ts}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolderWithPrefix(ctx, dir, "P/"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Uploaded != 1 {
		t.Fatalf("UploadChanged = %+v, %v; want 1 upload", res, err)
	}
	if _, ok := fake.Objects["P/a.txt"]; !ok {
		t.Errorf("objects = %v, want P/a.txt", keys(fake.Objects))
	}
}

// TestAdoptAfterPrefixChange: when a folder's prefix changes and the bucket
// already holds the files under the new names, they are adopted, not uploaded.
func TestAdoptAfterPrefixChange(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "same")
	if err := st.AddFolder(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}
	fake.ReportSHA1 = true
	fake.Objects["NEW/a.txt"] = []byte("same")
	if err := st.SetFolderPrefix(ctx, dir, "NEW/"); err != nil {
		t.Fatal(err)
	}
	fake.FailUpload = func(key string) error { return errors.New("must not upload " + key) }
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Adopted != 1 || res.Uploaded != 0 {
		t.Fatalf("UploadChanged = %+v, %v; want 1 adopted", res, err)
	}
}

// TestLegacyRelativeOuterFolderSkipsAbsoluteInner: an outer folder stored as a
// relative path (older bb) still skips an absolutely-stored nested folder, so
// its files are uploaded once, under the inner prefix.
func TestLegacyRelativeOuterFolderSkipsAbsoluteInner(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	base := t.TempDir()
	inner := filepath.Join(base, "docs", "SEWA")
	mkdirAll(t, inner)
	writeFile(t, filepath.Join(inner, "a.pdf"), "a")
	t.Chdir(base)
	if err := st.AddFolder(ctx, "docs"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolderWithPrefix(ctx, inner, "SEWA/"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Uploaded != 1 || len(fake.Objects) != 1 {
		t.Fatalf("UploadChanged = %+v, %v, objects %v; want one upload", res, err, keys(fake.Objects))
	}
	if _, ok := fake.Objects["SEWA/a.pdf"]; !ok {
		t.Errorf("objects = %v, want SEWA/a.pdf", keys(fake.Objects))
	}
}
