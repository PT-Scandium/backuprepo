package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/backup"
	"backuprepo/internal/store"
)

// TestNormalizeRemotePrefix covers what --remote accepts and how it is stored.
func TestNormalizeRemotePrefix(t *testing.T) {
	for in, want := range map[string]string{
		"MANDALATAMA-SEWA":   "MANDALATAMA-SEWA/",
		"MANDALATAMA-SEWA/":  "MANDALATAMA-SEWA/",
		" /office//sewa/ ":   "office/sewa/",
		`office\sewa`:        "office/sewa/",
		"Kontrak Sewa 2025/": "Kontrak Sewa 2025/",
	} {
		if got, err := normalizeRemotePrefix(in); err != nil || got != want {
			t.Errorf("normalizeRemotePrefix(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "  ", "/", "//", ".", "a/../b", "a/./b", "bad\x01name", strings.Repeat("x", maxPrefixLen)} {
		if got, err := normalizeRemotePrefix(in); !errors.Is(err, apperr.ErrInvalidPrefix) {
			t.Errorf("normalizeRemotePrefix(%q) = %q, %v; want ErrInvalidPrefix", in, got, err)
		}
	}
}

// TestWatchRemoteAddsFolderWithPrefix: a new folder is stored with its prefix,
// and list/config show it as "path -> prefix".
func TestWatchRemoteAddsFolderWithPrefix(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	var out bytes.Buffer
	if err := WatchRemote(ctx, st, dir, "SEWA", &out); err != nil {
		t.Fatalf("WatchRemote: %v", err)
	}
	if !strings.Contains(out.String(), "Watching "+dir+" -> SEWA/") {
		t.Errorf("output = %q", out.String())
	}
	got, _ := st.ListWatched(ctx)
	if len(got) != 1 || got[0] != (store.Folder{Path: dir, RemotePrefix: "SEWA/"}) {
		t.Fatalf("ListWatched = %+v", got)
	}
	out.Reset()
	if err := List(ctx, st, &out); err != nil || !strings.Contains(out.String(), "  "+dir+" -> SEWA/\n") {
		t.Errorf("List = %q, %v", out.String(), err)
	}
	if err := st.SaveConfig(ctx, store.RemoteConfig{KeyID: "k", AppKey: "secret", Bucket: "b"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Config(ctx, st, &out); err != nil || !strings.Contains(out.String(), "  "+dir+" -> SEWA/\n") {
		t.Errorf("Config = %q, %v", out.String(), err)
	}
}

// TestWatchRemoteSamePrefixIsNoop: repeating the same --remote changes nothing.
func TestWatchRemoteSamePrefixIsNoop(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := st.AddFolderWithPrefix(ctx, dir, "SEWA/"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WatchRemote(ctx, st, dir+string(filepath.Separator), "/SEWA/", &out); err != nil {
		t.Fatalf("WatchRemote: %v", err)
	}
	if !strings.Contains(out.String(), "Already watching "+dir+" -> SEWA/") {
		t.Errorf("output = %q", out.String())
	}
}

// TestWatchRemoteChangesPrefixAndWarns: on a watched folder --remote changes the
// prefix and says how many backed-up files will be uploaded again.
func TestWatchRemoteChangesPrefixAndWarns(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolder(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.New(st, b2.NewFake()).UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WatchRemote(ctx, st, dir, "NEW", &out); err != nil {
		t.Fatalf("WatchRemote: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "Watching "+dir+" -> NEW/ (bucket prefix changed)") ||
		!strings.Contains(s, "Note: 1 file(s) already backed up under other names will be uploaded again under NEW/") {
		t.Errorf("output = %q", s)
	}
	if got, _ := st.ListWatched(ctx); len(got) != 1 || got[0].RemotePrefix != "NEW/" {
		t.Errorf("ListWatched = %+v", got)
	}
}

// TestWatchRemoteNestedWarnsAboutRekey: putting a prefix on a subfolder of a
// folder already backed up re-keys its files, and the output says so.
func TestWatchRemoteNestedWarnsAboutRekey(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	docs := t.TempDir()
	sewa := filepath.Join(docs, "SEWA")
	if err := os.Mkdir(sewa, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sewa, "a.pdf"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "notes.md"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolder(ctx, docs); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.New(st, b2.NewFake()).UploadChanged(ctx); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WatchRemote(ctx, st, sewa, "SEWA", &out); err != nil {
		t.Fatalf("WatchRemote: %v", err)
	}
	if !strings.Contains(out.String(), "Note: 1 file(s)") {
		t.Errorf("output = %q, want a note about 1 re-keyed file (notes.md keeps its key)", out.String())
	}
}

// TestWatchRemoteRejectsOverlap: a prefix inside, or containing, another
// folder's object names is refused — the folders would overwrite each other.
func TestWatchRemoteRejectsOverlap(t *testing.T) {
	ctx := context.Background()
	plain := t.TempDir()
	for _, tc := range []struct {
		name   string
		other  store.Folder
		remote string
	}{
		{"inside another prefix", store.Folder{Path: "/x/office", RemotePrefix: "OFFICE/"}, "OFFICE/sewa"},
		{"contains another prefix", store.Folder{Path: "/x/office", RemotePrefix: "OFFICE/SEWA/"}, "OFFICE"},
		{"same prefix", store.Folder{Path: "/x/office", RemotePrefix: "SEWA/"}, "SEWA"},
		{"inside a plain folder's default names", store.Folder{Path: plain}, backup.RemoteKey(plain) + "/sub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			if err := st.AddFolderWithPrefix(ctx, tc.other.Path, tc.other.RemotePrefix); err != nil {
				t.Fatal(err)
			}
			err := WatchRemote(ctx, st, t.TempDir(), tc.remote, &bytes.Buffer{})
			if !errors.Is(err, apperr.ErrInvalidPrefix) {
				t.Fatalf("WatchRemote(%q) = %v, want ErrInvalidPrefix", tc.remote, err)
			}
			if got, _ := st.ListWatched(ctx); len(got) != 1 {
				t.Errorf("watch list changed: %+v", got)
			}
		})
	}
}

// TestWatchRemoteErrors: a bad prefix, a missing folder, and a folder watched as
// a legacy relative path are all refused without touching the watch list.
func TestWatchRemoteErrors(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if err := WatchRemote(ctx, st, t.TempDir(), "../up", &bytes.Buffer{}); !errors.Is(err, apperr.ErrInvalidPrefix) {
		t.Errorf("bad prefix: %v", err)
	}
	if err := WatchRemote(ctx, st, filepath.Join(t.TempDir(), "missing"), "P", &bytes.Buffer{}); !errors.Is(err, apperr.ErrFolderNotFound) {
		t.Errorf("missing folder: %v", err)
	}
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	if err := st.AddFolder(ctx, "proj"); err != nil {
		t.Fatal(err)
	}
	err := WatchRemote(ctx, st, filepath.Join(base, "proj"), "P", &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrInvalidPrefix) || !strings.Contains(err.Error(), "bb unwatch proj") {
		t.Errorf("legacy relative entry: %v", err)
	}
	if got, _ := st.ListWatched(ctx); len(got) != 1 || got[0].RemotePrefix != "" {
		t.Errorf("watch list changed: %+v", got)
	}
}

// TestUnwatchNestedFolderWarns: unwatching a folder that another watched folder
// contains says its files will be uploaded again under the outer names.
func TestUnwatchNestedFolderWarns(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	docs := t.TempDir()
	sewa := filepath.Join(docs, "SEWA")
	if err := os.Mkdir(sewa, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolder(ctx, docs); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolderWithPrefix(ctx, sewa, "SEWA/"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Unwatch(ctx, st, sewa, &out); err != nil {
		t.Fatalf("Unwatch: %v", err)
	}
	if !strings.Contains(out.String(), "Note: "+sewa+" is still backed up as part of "+docs) {
		t.Errorf("output = %q", out.String())
	}
	out.Reset()
	if err := Unwatch(ctx, st, docs, &out); err != nil || strings.Contains(out.String(), "Note:") {
		t.Errorf("unwatching a top-level folder: %q, %v; want no note", out.String(), err)
	}
}

// TestUploadReportsAdoptedFiles: `bb upload` says how many files were already
// in the bucket (recorded without an upload).
func TestUploadReportsAdoptedFiles(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolderWithPrefix(ctx, dir, "P/"); err != nil {
		t.Fatal(err)
	}
	fake := b2.NewFake()
	fake.ReportSHA1 = true
	fake.Objects["P/a.txt"] = []byte("same")
	var out bytes.Buffer
	if err := Upload(ctx, st, fake, false, &out); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !strings.Contains(out.String(), "Uploaded: 0, Skipped: 0, Deleted: 0, Failed: 0\nAlready in the bucket (recorded, not uploaded): 1\n") {
		t.Errorf("output = %q", out.String())
	}
}

// TestPlainWatchRespectsPrefixedFolders: a plain `bb watch` is refused when its
// default object names would collide with a prefixed folder's, and watching a
// subfolder of a prefixed folder plainly warns that its files are re-keyed.
func TestPlainWatchRespectsPrefixedFolders(t *testing.T) {
	ctx := context.Background()
	t.Run("collision refused", func(t *testing.T) {
		st := newStore(t)
		dir := t.TempDir()
		if err := st.AddFolderWithPrefix(ctx, "/data", backup.RemoteKey(dir)+"/"); err != nil {
			t.Fatal(err)
		}
		if err := Watch(ctx, st, dir, &bytes.Buffer{}); !errors.Is(err, apperr.ErrInvalidPrefix) {
			t.Fatalf("Watch = %v, want ErrInvalidPrefix", err)
		}
		if got, _ := st.ListWatched(ctx); len(got) != 1 {
			t.Errorf("watch list changed: %+v", got)
		}
	})
	t.Run("nested plain folder re-keys", func(t *testing.T) {
		st := newStore(t)
		outer := t.TempDir()
		inner := filepath.Join(outer, "sub")
		if err := os.Mkdir(inner, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inner, "a.txt"), []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := st.AddFolderWithPrefix(ctx, outer, "OUT/"); err != nil {
			t.Fatal(err)
		}
		if _, err := backup.New(st, b2.NewFake()).UploadChanged(ctx); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := Watch(ctx, st, inner, &out); err != nil {
			t.Fatalf("Watch: %v", err)
		}
		if !strings.Contains(out.String(), "Note: 1 file(s) already backed up under a bucket prefix") {
			t.Errorf("output = %q", out.String())
		}
	})
}

// lookupFailing is a backend whose SHA-1 lookups always fail.
type lookupFailing struct{ *b2.FakeBackend }

func (lookupFailing) RemoteSHA1(context.Context, string) (string, bool, error) {
	return "", false, errors.New("b2 unavailable")
}

// TestUploadReportsFailedHashLookups: `bb upload` says when files were uploaded
// because their hash couldn't be looked up (a possible needless new version).
func TestUploadReportsFailedHashLookups(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.AddFolderWithPrefix(ctx, dir, "P/"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Upload(ctx, st, lookupFailing{b2.NewFake()}, false, &out); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !strings.Contains(out.String(), "Uploaded: 1,") || !strings.Contains(out.String(), "Could not ask the bucket for 1 file(s)' hashes; uploaded them instead") {
		t.Errorf("output = %q", out.String())
	}
}
