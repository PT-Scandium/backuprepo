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

// TestUploadChangedLeavesSyncedFoldersToSync: a folder mapped to a bucket
// folder is kept in two-way sync by package syncer, so the one-way uploader
// never touches it (ADR-028) — not even a brand-new file.
func TestUploadChangedLeavesSyncedFoldersToSync(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	mkdirAll(t, filepath.Join(dir, "2025"))
	writeFile(t, filepath.Join(dir, "2025", "a.pdf"), "pdf")
	if err := st.AddFolderWithPrefix(ctx, dir, "SEWA/"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Uploaded != 0 || len(fake.Objects) != 0 {
		t.Fatalf("UploadChanged = %+v, %v, objects %v; want nothing uploaded", res, err, keys(fake.Objects))
	}
	if n, err := svc.PendingCount(ctx); err != nil || n != 1 {
		t.Errorf("PendingCount = %d, %v; want the synced folder's new file counted", n, err)
	}
}

// TestNestedSyncedFolderIsSkippedByOuterBackup: with ~/Documents backed up one
// way and ~/Documents/SEWA mapped (synced), the outer walk skips the inner
// folder entirely: its files are neither uploaded under their absolute names
// nor counted twice.
func TestNestedSyncedFolderIsSkippedByOuterBackup(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	docs := t.TempDir()
	sewa := filepath.Join(docs, "SEWA")
	mkdirAll(t, sewa)
	writeFile(t, filepath.Join(docs, "notes.md"), "n")
	writeFile(t, filepath.Join(sewa, "a.pdf"), "a")
	writeFile(t, filepath.Join(sewa, SyncTempPrefix+"x.tmp"), "half")
	if err := st.AddFolder(ctx, docs); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolderWithPrefix(ctx, sewa, "SEWA/"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Uploaded != 1 || len(fake.Objects) != 1 {
		t.Fatalf("UploadChanged = %+v, %v, objects %v; want only notes.md", res, err, keys(fake.Objects))
	}
	if _, ok := fake.Objects[RemoteKey(filepath.Join(docs, "notes.md"))]; !ok {
		t.Errorf("objects = %v, want notes.md under its absolute name", keys(fake.Objects))
	}
	if n, _ := svc.PendingCount(ctx); n != 1 {
		t.Errorf("PendingCount = %d, want 1 (SEWA/a.pdf once; the sync temp file never)", n)
	}
}

// TestOwner: the innermost watched folder owns a path; nothing owns a path
// outside every folder or a folder itself.
func TestOwner(t *testing.T) {
	docs := filepath.Join(t.TempDir(), "Documents")
	sewa := filepath.Join(docs, "SEWA")
	folders := []store.Folder{{Path: docs}, {Path: sewa, RemotePrefix: "SEWA/"}}
	if f, ok := Owner(folders, filepath.Join(sewa, "a.pdf")); !ok || f.Path != sewa || !f.Synced() {
		t.Errorf("Owner(SEWA/a.pdf) = %+v, %v", f, ok)
	}
	if f, ok := Owner(folders, filepath.Join(docs, "n.md")); !ok || f.Path != docs || f.Synced() {
		t.Errorf("Owner(n.md) = %+v, %v", f, ok)
	}
	if _, ok := Owner(folders, filepath.Join(t.TempDir(), "x")); ok {
		t.Error("a path outside every folder has no owner")
	}
	if f, ok := Owner(folders, sewa+"X"); !ok || f.Path != docs {
		t.Errorf("Owner(SEWAX) = %+v, %v; a sibling sharing a name prefix belongs to the outer folder", f, ok)
	}
	if got := KeyFor([]store.Folder{{Path: docs, RemotePrefix: store.RootPrefix}}, filepath.Join(sewa, "a.pdf")); got != "SEWA/a.pdf" {
		t.Errorf("KeyFor under a bucket-root folder = %q, want SEWA/a.pdf", got)
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
	fake.Objects[RemoteKey(file)] = []byte("contract")
	if err := st.AddFolder(ctx, dir); err != nil {
		t.Fatal(err)
	}
	fake.FailUpload = func(key string) error { return errors.New("must not upload " + key) }
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Adopted != 1 || res.Uploaded != 0 {
		t.Fatalf("UploadChanged = %+v, %v; want 1 adopted, 0 uploaded", res, err)
	}
	rec, _ := st.GetFile(ctx, file)
	if rec == nil || rec.LastBackup == nil || rec.RemoteKey != RemoteKey(file) {
		t.Fatalf("record = %+v, want backed up under its name", rec)
	}
	if res, err := svc.UploadChanged(ctx); err != nil || res.Skipped != 1 || res.Adopted != 0 {
		t.Errorf("second run = %+v, %v; want 1 skipped", res, err)
	}
}

// TestAdoptFallsBackToUpload: whenever adopting can't prove the bucket holds
// the same bytes, the file is uploaded — different content, a hash B2 can't
// compare ("none" for large files), a backend without the capability, or a
// failed hash lookup (counted).
func TestAdoptFallsBackToUpload(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *b2.FakeBackend, key string)
		up    func(f *b2.FakeBackend) b2.Uploader
	}{
		{"different content", func(f *b2.FakeBackend, key string) { f.ReportSHA1 = true; f.Objects[key] = []byte("old") }, nil},
		{"large-file sha none", func(f *b2.FakeBackend, key string) {
			f.ReportSHA1 = true
			f.Objects[key] = []byte("new")
			f.SHA1Override = map[string]string{key: "none"}
		}, nil},
		{"backend without hashes", func(f *b2.FakeBackend, key string) { f.Objects[key] = []byte("new") }, nil},
		{"hash lookup fails", func(f *b2.FakeBackend, key string) { f.Objects[key] = []byte("new") },
			func(f *b2.FakeBackend) b2.Uploader { return failingHasher{f} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st, fake := newSvc(t)
			ctx := context.Background()
			dir := t.TempDir()
			file := filepath.Join(dir, "a.txt")
			key := RemoteKey(file)
			writeFile(t, file, "new")
			tc.setup(fake, key)
			var up b2.Uploader = fake
			if tc.up != nil {
				up = tc.up(fake)
			}
			svc := New(st, up)
			if err := st.AddFolder(ctx, dir); err != nil {
				t.Fatal(err)
			}
			res, err := svc.UploadChanged(ctx)
			if err != nil || res.Uploaded != 1 || res.Adopted != 0 {
				t.Fatalf("UploadChanged = %+v, %v; want 1 upload, 0 adopted", res, err)
			}
			if wantFail := tc.name == "hash lookup fails"; (res.HashLookupsFailed == 1) != wantFail {
				t.Errorf("HashLookupsFailed = %d, want %v", res.HashLookupsFailed, wantFail)
			}
			if string(fake.Objects[key]) != "new" {
				t.Errorf("remote = %q, want the new content", fake.Objects[key])
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
	if err := st.AddFolder(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}
	writeFile(t, file, "v2-longer")
	// Even if the server claimed to hold the new bytes, an edit must upload.
	fake.ReportSHA1 = true
	fake.SHA1Override = map[string]string{RemoteKey(file): sha1Hex("v2-longer")}
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
// stored was backed up under the default name, so it counts as backed up only
// under that name — never under a prefixed one.
func TestUnkeyedRecordMatchesOnlyDefaultName(t *testing.T) {
	ts := int64(1)
	rec := &store.FileRecord{Path: "/data/a.txt", LastBackup: &ts}
	if !backedUpUnder(rec, RemoteKey("/data/a.txt")) {
		t.Error("an unkeyed record must match its default name")
	}
	if backedUpUnder(rec, "P/a.txt") {
		t.Error("an unkeyed record must not match a prefixed name")
	}
	if backedUpUnder(&store.FileRecord{Path: "/data/a.txt", RemoteKey: "P/a.txt"}, "P/a.txt") {
		t.Error("a record never backed up (no LastBackup) is not backed up")
	}
}

// TestAdoptPlainFileAfterRecordLoss: a plain-folder file whose record is gone
// (e.g. a fresh database) but whose exact bytes are already in the bucket
// under its name is adopted, not uploaded again.
func TestAdoptPlainFileAfterRecordLoss(t *testing.T) {
	svc, st, fake := newSvc(t)
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	writeFile(t, file, "same")
	if err := st.AddFolder(ctx, dir); err != nil {
		t.Fatal(err)
	}
	fake.ReportSHA1 = true
	fake.Objects[RemoteKey(file)] = []byte("same")
	fake.FailUpload = func(key string) error { return errors.New("must not upload " + key) }
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Adopted != 1 || res.Uploaded != 0 {
		t.Fatalf("UploadChanged = %+v, %v; want 1 adopted", res, err)
	}
}

// TestLegacyRelativeOuterFolderSkipsAbsoluteInner: an outer folder stored as a
// relative path (older bb) still skips an absolutely-stored nested synced
// folder, so its files are not uploaded under the outer folder's names.
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
	if err != nil || res.Uploaded != 0 || len(fake.Objects) != 0 {
		t.Fatalf("UploadChanged = %+v, %v, objects %v; want nothing (inner folder is synced)", res, err, keys(fake.Objects))
	}
}

// TestDeletePropagationSparesMappedFolders: `upload --delete` never deletes
// anything for a mapped (two-way synced) folder — not for a mapped folder of
// its own, nor one nested in a one-way folder (ADR-028: sync never deletes).
func TestDeletePropagationSparesMappedFolders(t *testing.T) {
	_, st, fake := newSvc(t)
	svc := New(st, fake).WithDeleter(fake)
	ctx := context.Background()
	docs := t.TempDir()
	sewa := filepath.Join(docs, "SEWA")
	mkdirAll(t, sewa)
	if err := st.AddFolder(ctx, docs); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolderWithPrefix(ctx, sewa, "SEWA/"); err != nil {
		t.Fatal(err)
	}
	ts := int64(1)
	for _, rec := range []store.FileRecord{
		{Path: filepath.Join(sewa, "gone.pdf"), LastBackup: &ts, RemoteKey: "SEWA/gone.pdf"},
		{Path: filepath.Join(docs, "gone.md"), LastBackup: &ts, RemoteKey: RemoteKey(filepath.Join(docs, "gone.md"))},
	} {
		fake.Objects[rec.RemoteKey] = []byte("x")
		if err := st.UpsertFile(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	res, err := svc.UploadChanged(ctx)
	if err != nil || res.Deleted != 1 {
		t.Fatalf("UploadChanged = %+v, %v; want only the one-way folder's file deleted", res, err)
	}
	if _, ok := fake.Objects["SEWA/gone.pdf"]; !ok {
		t.Error("the mapped folder's object was deleted")
	}
}
