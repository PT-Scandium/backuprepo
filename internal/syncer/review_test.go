package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/store"
)

// TestConflictsNeverOverwriteEachOther: two mapped folders losing a conflict
// on the same relative path in one run (same timestamp), and a second run in
// the same second, keep every losing local version.
func TestConflictsNeverOverwriteEachOther(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake := b2.NewFake()
	now := time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)
	opt := Options{ConflictDir: filepath.Join(t.TempDir(), "conflicts"), Now: func() time.Time { return now }}
	write := func(dir, content string, mt time.Time) {
		p := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	// Two folders with the same base name, in different parents.
	a := filepath.Join(t.TempDir(), "same")
	b := filepath.Join(t.TempDir(), "same")
	for i, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := st.AddFolderWithPrefix(ctx, d, []string{"A/", "B/"}[i]); err != nil {
			t.Fatal(err)
		}
		write(d, "base", t0)
	}
	if _, err := All(ctx, st, fake, opt); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 2; round++ { // two conflict runs in the same second
		for i, d := range []string{a, b} {
			write(d, "local-"+string(rune('A'+i))+"-edit", t0.Add(time.Duration(round)*time.Hour))
			key := []string{"A/f.txt", "B/f.txt"}[i]
			if err := fake.UploadWithModTime(ctx, key, strings.NewReader("remote"), 6, t0.Add(time.Duration(round)*time.Hour+time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := All(ctx, st, fake, opt); err != nil {
			t.Fatal(err)
		}
	}
	var kept []string
	_ = filepath.WalkDir(opt.ConflictDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			kept = append(kept, string(b))
		}
		return nil
	})
	if len(kept) != 4 {
		t.Fatalf("kept-aside versions = %q, want 4 (2 folders x 2 runs)", kept)
	}
}

// TestFreeName never returns an existing file, shortens over-long names
// instead of failing on them, and always terminates.
func TestFreeName(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if got, err := freeName(p); err != nil || got != p {
		t.Fatalf("free path: %q, %v", got, err)
	}
	_ = os.WriteFile(p, nil, 0o644)
	if got, _ := freeName(p); got != filepath.Join(dir, "a (2).txt") {
		t.Fatalf("freeName = %q", got)
	}
	_ = os.WriteFile(filepath.Join(dir, "a (2).txt"), nil, 0o644)
	if got, _ := freeName(p); got != filepath.Join(dir, "a (3).txt") {
		t.Fatalf("freeName = %q", got)
	}
	// A 255-byte name already present: "<stem> (2).ext" would exceed the
	// filesystem's name limit (ENAMETOOLONG on every try — this used to loop
	// forever, hanging the daemon under its lock).
	long := filepath.Join(dir, strings.Repeat("x", 251)+".pdf")
	if err := os.WriteFile(long, []byte("x"), 0o644); err != nil {
		t.Skipf("filesystem rejects a 255-byte name: %v", err)
	}
	done := make(chan struct{})
	var got string
	var err error
	go func() { got, err = freeName(long); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("freeName did not return for a 255-byte name")
	}
	if err != nil || len(filepath.Base(got)) > 255 || !strings.HasSuffix(got, " (2).pdf") {
		t.Fatalf("freeName(255-byte name) = %q (base %d bytes), %v", got, len(filepath.Base(got)), err)
	}
	if err := os.WriteFile(got, []byte("y"), 0o644); err != nil {
		t.Fatalf("the shortened name can't be created: %v", err)
	}
}

// TestNestedFolderRemoteObjectsExcluded: a watched folder nested in a mapped
// folder owns its part of the bucket folder: those objects are neither
// downloaded into it nor touched, in both directions.
func TestNestedFolderRemoteObjectsExcluded(t *testing.T) {
	e := newEnv(t, store.RootPrefix)
	sub := filepath.Join(e.root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddFolder(e.ctx, sub); err != nil {
		t.Fatal(err)
	}
	e.putRemote("sub/a.txt", "would land in the one-way folder", t0)
	e.putRemote("top.txt", "t", t0)
	e.resetLog()
	got := actions(t, e.sync(false))
	if len(got) != 1 || got["top.txt"] != Download {
		t.Fatalf("actions = %v; want only top.txt", got)
	}
	if _, err := os.Stat(filepath.Join(sub, "a.txt")); !os.IsNotExist(err) {
		t.Error("an object of the nested folder's part of the bucket was downloaded into it")
	}
}

// TestEmptiedFolderIsNotRefilled: when every file synced before is missing
// locally (an unmounted drive's empty mount point), sync refuses unless
// refilling is allowed.
func TestEmptiedFolderIsNotRefilled(t *testing.T) {
	e := newEnv(t, "P/")
	e.writeLocal("a.txt", "a", t0)
	e.writeLocal("d/b.txt", "b", t0)
	e.sync(false)
	if err := os.RemoveAll(filepath.Join(e.root, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	e.resetLog()
	folders, _ := e.st.ListWatched(e.ctx)
	_, err := Folder(e.ctx, e.st, e.fake, folders, e.folder, Options{ConflictDir: e.conflict})
	if !errors.Is(err, apperr.ErrSync) || !strings.Contains(err.Error(), "--refill") || len(e.fake.Downloads) != 0 {
		t.Fatalf("Folder = %v, downloads %v; want a refusal mentioning --refill", err, e.fake.Downloads)
	}
	rep, err := Folder(e.ctx, e.st, e.fake, folders, e.folder, Options{ConflictDir: e.conflict, AllowRefill: true})
	if err != nil || rep.Downloaded != 2 {
		t.Fatalf("with AllowRefill: %+v, %v", rep, err)
	}
	// Gone on both sides (deleted outside bb): forgotten, not refused.
	for _, rel := range []string{"a.txt", "d/b.txt"} {
		_ = os.Remove(filepath.Join(e.root, filepath.FromSlash(rel)))
		_ = e.fake.Delete(e.ctx, "P/"+rel)
	}
	if got := actions(t, e.sync(false)); got["a.txt"] != Forget || got["d/b.txt"] != Forget {
		t.Fatalf("gone on both sides: actions = %v, want forget", got)
	}
	e.writeLocal("a.txt", "a", t0)
	e.writeLocal("d/b.txt", "b", t0)
	e.sync(false)
	// One file missing out of several is an ordinary restore, not a refusal.
	if err := os.Remove(filepath.Join(e.root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if got := actions(t, e.sync(false)); got["a.txt"] != Download {
		t.Errorf("actions = %v", got)
	}
}

// TestRemoteChanged covers the change tests: version id, SHA-1, bb's own
// upload not yet listed (also when B2 reports no hash), and the upload-time
// fallback, which must not use a colleague's older source time.
func TestRemoteChanged(t *testing.T) {
	last := int64(1_800_000_000)
	lb := &last
	for _, tc := range []struct {
		name string
		base store.FileRecord
		obj  b2.ObjectInfo
		want bool
	}{
		{"same id", store.FileRecord{RemoteFileID: "v1"}, b2.ObjectInfo{FileID: "v1"}, false},
		{"new id", store.FileRecord{RemoteFileID: "v1"}, b2.ObjectInfo{FileID: "v2"}, true},
		{"own upload, same sha", store.FileRecord{RemoteSHA1: "aa"}, b2.ObjectInfo{FileID: "v9", SHA1: "AA"}, false},
		{"own upload, other sha", store.FileRecord{RemoteSHA1: "aa"}, b2.ObjectInfo{FileID: "v9", SHA1: "bb"}, true},
		{"own large upload, no hash, same size", store.FileRecord{RemoteSHA1: "aa", Size: 5}, b2.ObjectInfo{FileID: "v9", Size: 5}, false},
		{"own large upload, no hash, other size", store.FileRecord{RemoteSHA1: "aa", Size: 5}, b2.ObjectInfo{FileID: "v9", Size: 6}, true},
		{"plain record, uploaded later with an old source time", store.FileRecord{LastBackup: lb},
			b2.ObjectInfo{Modified: time.Unix(last+60, 0), SrcModified: time.Unix(last-3600, 0)}, true},
		{"plain record, uploaded before", store.FileRecord{LastBackup: lb}, b2.ObjectInfo{Modified: time.Unix(last-60, 0)}, false},
	} {
		if got := remoteChanged(remoteFile{key: "k", obj: tc.obj}, tc.base); got != tc.want {
			t.Errorf("%s: remoteChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !remoteChanged(remoteFile{}, store.FileRecord{RemoteFileID: "v1"}) {
		t.Error("a missing remote counts as changed")
	}
}

// noHashBackend reports SHA-1 "none" for every object, as B2 does for large
// files.
type noHashBackend struct{ *b2.FakeBackend }

func (n noHashBackend) List(ctx context.Context, prefix string, recursive bool) (b2.Listing, error) {
	l, err := n.FakeBackend.List(ctx, prefix, recursive)
	for i := range l.Objects {
		l.Objects[i].SHA1 = "none"
	}
	return l, err
}

// TestLargeFileUploadNotDownloadedBack: a file bb uploaded whose listing shows
// no hash ("none") isn't downloaded back or re-uploaded on the next runs.
func TestLargeFileUploadNotDownloadedBack(t *testing.T) {
	e := newEnv(t, "P/")
	e.writeLocal("big.bin", "pretend this is 200 MB", t0)
	be := noHashBackend{e.fake}
	folders, _ := e.st.ListWatched(e.ctx)
	opt := Options{ConflictDir: e.conflict}
	if rep, err := Folder(e.ctx, e.st, be, folders, e.folder, opt); err != nil || rep.Uploaded != 1 {
		t.Fatalf("first run: %+v, %v", rep, err)
	}
	e.resetLog()
	for i := 0; i < 2; i++ {
		rep, err := Folder(e.ctx, e.st, be, folders, e.folder, opt)
		if err != nil || len(rep.Items) != 0 || len(e.fake.Uploads)+len(e.fake.Downloads) != 0 {
			t.Fatalf("run %d: %+v, %v (up %v down %v); want nothing", i+2, rep.Items, err, e.fake.Uploads, e.fake.Downloads)
		}
	}
}

// TestSafeRelOnWindows refuses names NTFS can't hold (":" opens a stream).
func TestSafeRelOnWindows(t *testing.T) {
	for _, rel := range []string{"a:b.txt", "dir/x:stream", "q?.txt", "p|q"} {
		if safeRelFor(rel, "windows") {
			t.Errorf("safeRelFor(%q, windows) = true", rel)
		}
		if !safeRelFor(rel, "linux") {
			t.Errorf("safeRelFor(%q, linux) = false", rel)
		}
	}
	if !isSHA1Hex(strings.Repeat("a", 40)) || isSHA1Hex("none") || isSHA1Hex(strings.Repeat("z", 40)) {
		t.Error("isSHA1Hex")
	}
}

// TestLockIsExclusive: a second Lock on the same folder fails with ErrBusy
// until the first is released.
func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	// flock is per open file description, so a second open in the same
	// process contends like another process would.
	if _, err := Lock(dir); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Lock = %v, want ErrBusy", err)
	}
	unlock()
	unlock2, err := Lock(dir)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	unlock2()
}

// freeNameWithin runs freeName with a deadline (a bad shortening loop would
// otherwise hang the test, as it would hang the daemon).
func freeNameWithin(t *testing.T, p string) (string, error) {
	t.Helper()
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() { s, err := freeName(p); ch <- res{s, err} }()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(5 * time.Second):
		t.Fatalf("freeName(%q) did not return", filepath.Base(p))
		return "", nil
	}
}

// TestFreeNameMultiByteAndLongExtension: shortening keeps whole runes (a
// conflict copy of a Japanese or accented name stays valid UTF-8), and a very
// long "extension" is treated as part of the name instead of being kept whole.
func TestFreeNameMultiByteAndLongExtension(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		strings.Repeat("あ", 84) + ".md",       // 255 bytes, 3-byte runes
		"x" + strings.Repeat("é", 126) + ".x", // 255 bytes, 2-byte runes at an odd offset
		"a." + strings.Repeat("e", 252),       // 254 bytes, 253-byte "extension"
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Skipf("filesystem rejects a %d-byte name: %v", len(name), err)
		}
		got, err := freeNameWithin(t, p)
		base := filepath.Base(got)
		if err != nil || len(base) > 255 || !utf8.ValidString(base) || !strings.Contains(base, " (2)") {
			t.Fatalf("freeName(%d-byte name) = %q (%d bytes, valid UTF-8 %v), %v",
				len(name), base, len(base), utf8.ValidString(base), err)
		}
		if err := os.WriteFile(got, []byte("y"), 0o644); err != nil {
			t.Fatalf("the shortened name can't be created: %v", err)
		}
	}
}
