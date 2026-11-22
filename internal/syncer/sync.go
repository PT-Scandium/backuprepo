package syncer

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/backup"
	"backuprepo/internal/store"
)

// FolderMarker is the zero-byte object the Backblaze web UI (and `bb mapto`)
// creates to make an empty bucket folder visible. Sync never transfers it.
const FolderMarker = ".bzEmpty"

// Options tune one sync run.
type Options struct {
	// DryRun decides every file but changes nothing: no transfer, no local
	// write, no snapshot update.
	DryRun bool
	// ConflictDir receives local versions that lost a conflict, under
	// <ConflictDir>/<time>/<relative path>. Required unless DryRun.
	ConflictDir string
	// Now returns the current time (tests override it).
	Now func() time.Time
	// AllowRefill lets a run download into a folder where every previously
	// synced file is missing. Without it such a run is refused: the folder is
	// more likely an unmounted drive's empty mount point than emptied on
	// purpose, and refilling it would hide that (`bb sync --refill`).
	AllowRefill bool
}

// Item is one file's decision in a run.
type Item struct {
	Rel    string // path relative to the synced folder, "/"-separated
	Action Action
	Err    error // non-nil if executing the action failed
}

// Report summarizes one folder's sync.
type Report struct {
	Folder     store.Folder
	Items      []Item // every file whose action isn't None
	Uploaded   int
	Downloaded int
	Recorded   int
	Conflicts  int
	Failed     int
}

// remoteFile is a listed object under the synced prefix.
type remoteFile struct {
	key string
	obj b2.ObjectInfo
}

// localFile is a regular file under the synced folder.
type localFile struct {
	path string
	info fs.FileInfo
}

// All syncs every watched folder mapped to the bucket (Folder.Synced) and
// returns one report per folder. A folder that can't be synced (e.g. missing)
// gets an error but doesn't stop the others; the returned error joins them.
func All(ctx context.Context, st *store.Store, be b2.Backend, opt Options) ([]Report, error) {
	folders, err := st.ListWatched(ctx)
	if err != nil {
		return nil, err
	}
	var reports []Report
	var errs []error
	for _, f := range folders {
		if !f.Synced() {
			continue
		}
		rep, err := Folder(ctx, st, be, folders, f, opt)
		reports = append(reports, rep)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return reports, errors.Join(errs...)
}

// Folder syncs one mapped folder f with its bucket folder. folders is the full
// watch list: other folders nested in f are skipped (they are synced or backed
// up on their own) and other folders' object names are never touched.
func Folder(ctx context.Context, st *store.Store, be b2.Backend, folders []store.Folder, f store.Folder, opt Options) (Report, error) {
	rep := Report{Folder: f}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if !opt.DryRun && opt.ConflictDir == "" {
		return rep, fmt.Errorf("%w: no conflicts folder configured", apperr.ErrSync)
	}
	root := filepath.Clean(f.Path)
	// Never create a missing root: an unmounted drive must not be refilled
	// from the bucket (or read as "everything deleted").
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return rep, fmt.Errorf("%w: %s is missing or not a folder; not syncing it", apperr.ErrSync, root)
	}
	prefix := f.RemotePrefix
	if prefix == store.RootPrefix {
		prefix = ""
	}
	var skipDirs, exclude []string
	for _, o := range folders {
		if samePath(filepath.Clean(o.Path), root) {
			continue
		}
		if r, err := filepath.Rel(root, filepath.Clean(o.Path)); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			// A folder nested in this one is synced or backed up on its own:
			// skip it locally AND leave its part of this bucket folder alone,
			// or its objects would be downloaded into it (and uploaded again
			// under its own names).
			skipDirs = append(skipDirs, filepath.Clean(o.Path))
			exclude = append(exclude, prefix+filepath.ToSlash(r)+"/")
		}
		if ks := backup.KeySpace(o); ks != prefix && strings.HasPrefix(ks, prefix) {
			exclude = append(exclude, ks)
		}
	}
	excluded := func(key string) bool {
		for _, e := range exclude {
			if strings.HasPrefix(key, e) {
				return true
			}
		}
		return path.Base(key) == FolderMarker
	}

	remote, err := listRemote(ctx, be, prefix, excluded, &rep)
	if err != nil {
		return rep, err
	}
	local, err := walkLocal(root, prefix, skipDirs, excluded)
	if err != nil {
		return rep, err
	}
	base, err := snapshots(ctx, st, root, prefix, skipDirs)
	if err != nil {
		return rep, err
	}
	if !opt.AllowRefill && len(base) > 0 {
		// Refuse only a folder whose synced files are all gone locally while
		// some still exist remotely: that is what an empty mount point looks
		// like. Files gone on both sides are simply forgotten below.
		present, remoteLeft := false, false
		for rel := range base {
			if _, ok := local[rel]; ok {
				present = true
				break
			}
			if _, ok := remote[rel]; ok {
				remoteLeft = true
			}
		}
		if !present && remoteLeft {
			return rep, fmt.Errorf("%w: none of the %d files synced before are in %s — is a drive unmounted? "+
				"Not refilling it; if you emptied it on purpose, run `bb sync --refill` in it", apperr.ErrSync, len(base), root)
		}
	}

	rels := map[string]bool{}
	for r := range remote {
		rels[r] = true
	}
	for r := range local {
		rels[r] = true
	}
	for r := range base {
		rels[r] = true
	}
	sorted := make([]string, 0, len(rels))
	for r := range rels {
		sorted = append(sorted, r)
	}
	sort.Strings(sorted)

	// Losing local versions go to <conflicts>/<time>/<folder name>/<rel>;
	// setAside never overwrites, so runs in the same second or folders with the
	// same name can't clobber each other's kept-aside files.
	stamp := filepath.Join(opt.Now().Format("20060102-150405"), folderLabel(root))
	for _, rel := range sorted {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		item := syncOne(ctx, st, be, root, prefix, rel, local[rel], remote[rel], base[rel], opt, stamp)
		if item.Action == None && item.Err == nil {
			continue
		}
		rep.Items = append(rep.Items, item)
		switch {
		case item.Err != nil:
			rep.Failed++
		case opt.DryRun:
		case item.Action == Upload:
			rep.Uploaded++
		case item.Action == Download:
			rep.Downloaded++
		case item.Action == KeepLocal:
			rep.Uploaded++
			rep.Conflicts++
		case item.Action == KeepRemote:
			rep.Downloaded++
			rep.Conflicts++
		case item.Action == Record:
			rep.Recorded++
		}
	}
	return rep, nil
}

// folderLabel names a synced folder inside the conflicts folder: its base
// name, or "root" for a filesystem root.
func folderLabel(root string) string {
	b := filepath.Base(root)
	if b == "." || b == string(filepath.Separator) || b == "" || strings.HasSuffix(b, ":\\") {
		return "root"
	}
	return b
}

// listRemote lists the bucket under prefix, keyed by path relative to it.
// Folder entries, folder markers, other folders' objects and unsafe names
// (absolute, or climbing out with "..") are left out; an unsafe name is
// reported as a failed item.
func listRemote(ctx context.Context, be b2.Backend, prefix string, excluded func(string) bool, rep *Report) (map[string]remoteFile, error) {
	l, err := be.List(ctx, prefix, true)
	if err != nil {
		return nil, fmt.Errorf("%w: list %q: %v", apperr.ErrSync, prefix, err)
	}
	out := map[string]remoteFile{}
	for _, o := range l.Objects {
		rel := strings.TrimPrefix(o.Key, prefix)
		if rel == "" || strings.HasSuffix(rel, "/") || excluded(o.Key) {
			continue
		}
		if !safeRel(rel) {
			rep.Items = append(rep.Items, Item{Rel: rel, Action: Download,
				Err: fmt.Errorf("%w: unsafe object name %q skipped", apperr.ErrSync, o.Key)})
			rep.Failed++
			continue
		}
		if !isSHA1Hex(o.SHA1) {
			o.SHA1 = "" // "none" (large files), "unverified:…": unknown, never compared
		}
		out[rel] = remoteFile{key: o.Key, obj: o}
	}
	return out, nil
}

// safeRel reports whether rel is a clean relative "/" path that stays inside
// the synced folder once joined to it, on this OS.
func safeRel(rel string) bool { return safeRelFor(rel, runtime.GOOS) }

// safeRelFor is safeRel for the given OS. On Windows it also refuses the
// characters NTFS forbids (":" would even open an alternate data stream).
func safeRelFor(rel, goos string) bool {
	if rel != path.Clean(rel) || path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") ||
		strings.Contains(rel, "\\") || filepath.IsAbs(filepath.FromSlash(rel)) || filepath.VolumeName(filepath.FromSlash(rel)) != "" {
		return false
	}
	if goos == "windows" && strings.ContainsAny(rel, `:*?"<>|`) {
		return false
	}
	return true
}

// isSHA1Hex reports whether s is a 40-digit hex SHA-1.
func isSHA1Hex(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// walkLocal returns the regular files under root keyed by "/" relative path,
// skipping nested watched folders, sync temp files and excluded names.
func walkLocal(root, prefix string, skipDirs []string, excluded func(string) bool) (map[string]localFile, error) {
	out := map[string]localFile{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: leave it out of this run
		}
		if d.IsDir() {
			for _, s := range skipDirs {
				if samePath(filepath.Clean(p), s) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() || backup.IsSyncTemp(d.Name()) {
			return nil
		}
		r, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel := filepath.ToSlash(r)
		if excluded(prefix + rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out[rel] = localFile{path: p, info: info}
		return nil
	})
	return out, err
}

// snapshots returns the records of files under root (outside nested watched
// folders) whose stored key is the one this mapping gives them; a record kept
// under another key (e.g. before the folder was mapped) is no snapshot here.
func snapshots(ctx context.Context, st *store.Store, root, prefix string, skipDirs []string) (map[string]store.FileRecord, error) {
	files, err := st.ListFiles(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]store.FileRecord{}
	for _, fr := range files {
		r, err := filepath.Rel(root, fr.Path)
		if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			continue
		}
		nested := false
		for _, s := range skipDirs {
			if q, err := filepath.Rel(s, fr.Path); err == nil && q != ".." && !strings.HasPrefix(q, ".."+string(filepath.Separator)) {
				nested = true
				break
			}
		}
		rel := filepath.ToSlash(r)
		if nested || fr.LastBackup == nil || fr.RemoteKey != prefix+rel {
			continue
		}
		out[rel] = fr
	}
	return out, nil
}

// syncOne decides one file and, unless dry-running, carries the decision out.
func syncOne(ctx context.Context, st *store.Store, be b2.Backend, root, prefix, rel string,
	lf localFile, rf remoteFile, base store.FileRecord, opt Options, stamp string) Item {

	item := Item{Rel: rel}
	in := Input{
		HasBase:      base.Path != "",
		LocalExists:  lf.info != nil,
		RemoteExists: rf.key != "",
	}
	localPath := filepath.Join(root, filepath.FromSlash(rel))
	key := prefix + rel
	var h hashes // local hashes, computed only when needed
	hashed := false
	hashLocal := func() error {
		if hashed {
			return nil
		}
		var err error
		h, err = hashFile(lf.path)
		hashed = err == nil
		return err
	}
	if in.LocalExists {
		in.LocalTime = lf.info.ModTime()
	}
	if in.RemoteExists {
		in.RemoteTime = remoteTime(rf.obj)
	}
	if in.HasBase {
		if !in.LocalExists {
			in.LocalChanged = true
		} else if lf.info.Size() != base.Size || lf.info.ModTime().Unix() != base.ModTime {
			if err := hashLocal(); err != nil {
				item.Action, item.Err = Upload, fmt.Errorf("%w: hash %s: %v", apperr.ErrSync, rel, err)
				return item
			}
			in.LocalChanged = h.sha256 != base.SHA256
		}
		in.RemoteChanged = remoteChanged(rf, base)
	}
	if in.LocalExists && in.RemoteExists && (!in.HasBase || in.LocalChanged || in.RemoteChanged) &&
		lf.info.Size() == rf.obj.Size && rf.obj.SHA1 != "" {
		if err := hashLocal(); err != nil {
			item.Action, item.Err = Upload, fmt.Errorf("%w: hash %s: %v", apperr.ErrSync, rel, err)
			return item
		}
		in.SameContent = strings.EqualFold(h.sha1, rf.obj.SHA1)
	}
	item.Action = Decide(in)

	if opt.DryRun {
		return item
	}
	switch item.Action {
	case None:
		// Learn the id of a version bb uploaded itself (unknown until listed).
		if in.HasBase && base.RemoteFileID == "" && rf.obj.FileID != "" {
			base.RemoteFileID = rf.obj.FileID
			base.RemoteTime = in.RemoteTime.UnixMilli()
			_ = st.UpsertFile(ctx, base)
		}
	case Record:
		if err := hashLocal(); err != nil {
			item.Err = err
			return item
		}
		item.Err = st.UpsertFile(ctx, recordFor(localPath, lf.info, h, key, rf.obj, in.RemoteTime, opt.Now()))
	case Upload, KeepLocal:
		item.Err = upload(ctx, st, be, localPath, key, opt.Now())
	case Download:
		item.Err = download(ctx, st, be, localPath, rf, lf.info, opt.Now())
	case KeepRemote:
		if err := setAside(lf.path, filepath.Join(opt.ConflictDir, stamp, filepath.FromSlash(rel))); err != nil {
			item.Err = fmt.Errorf("%w: keep local %s aside: %v", apperr.ErrSync, rel, err)
			return item
		}
		item.Err = download(ctx, st, be, localPath, rf, nil, opt.Now())
	case Forget:
		item.Err = st.RemoveFile(ctx, base.Path)
	}
	return item
}

// remoteChanged reports whether the listed remote version differs from the
// snapshot: by version id when known, else by content hash, else by time.
func remoteChanged(rf remoteFile, base store.FileRecord) bool {
	switch {
	case rf.key == "":
		return true
	case base.RemoteFileID != "" && rf.obj.FileID != "":
		return rf.obj.FileID != base.RemoteFileID
	case base.RemoteSHA1 != "" && rf.obj.SHA1 != "":
		return !strings.EqualFold(rf.obj.SHA1, base.RemoteSHA1)
	case base.RemoteFileID == "" && base.RemoteSHA1 != "":
		// bb uploaded it and hasn't listed it since, and B2 reports no hash
		// (a large file): it's ours unless its size differs.
		return rf.obj.Size != base.Size
	}
	// A snapshot without remote details (a plain backup record): the remote
	// changed if it was uploaded after that backup. The upload time, not the
	// source time: a colleague may upload a file with an older source time.
	return base.LastBackup == nil || rf.obj.Modified.Unix() > *base.LastBackup
}

// remoteTime is the remote version's source modification time when recorded,
// else its upload time.
func remoteTime(o b2.ObjectInfo) time.Time {
	if !o.SrcModified.IsZero() {
		return o.SrcModified
	}
	return o.Modified
}

// recordFor builds the snapshot of a file that matches remote version o.
func recordFor(p string, info fs.FileInfo, h hashes, key string, o b2.ObjectInfo, rt, now time.Time) store.FileRecord {
	ts := now.Unix()
	return store.FileRecord{
		Path: p, Size: info.Size(), ModTime: info.ModTime().Unix(), SHA256: h.sha256,
		LastBackup: &ts, RemoteKey: key,
		RemoteFileID: o.FileID, RemoteSHA1: h.sha1, RemoteTime: rt.UnixMilli(),
	}
}

// upload sends the local file to key with its modification time and records
// the snapshot. The remote version id is learned at the next listing.
func upload(ctx context.Context, st *store.Store, be b2.Backend, p, key string, now time.Time) error {
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("%w: %v", apperr.ErrSync, err)
	}
	h, err := hashFile(p)
	if err != nil {
		return fmt.Errorf("%w: hash %s: %v", apperr.ErrSync, p, err)
	}
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("%w: %v", apperr.ErrSync, err)
	}
	defer f.Close()
	if mu, ok := be.(b2.ModTimeUploader); ok {
		err = mu.UploadWithModTime(ctx, key, f, info.Size(), info.ModTime())
	} else {
		err = be.Upload(ctx, key, f, info.Size())
	}
	if err != nil {
		return err
	}
	ts := now.Unix()
	return st.UpsertFile(ctx, store.FileRecord{
		Path: p, Size: info.Size(), ModTime: info.ModTime().Unix(), SHA256: h.sha256,
		LastBackup: &ts, RemoteKey: key,
		RemoteSHA1: h.sha1, RemoteTime: info.ModTime().UnixMilli(),
	})
}

// download fetches rf into p atomically: into a temp file next to it, then a
// rename, after checking p wasn't changed meanwhile (seen is what the
// decision saw; nil means p must not exist). The file gets the remote
// version's time, and the snapshot is recorded.
func download(ctx context.Context, st *store.Store, be b2.Backend, p string, rf remoteFile, seen fs.FileInfo, now time.Time) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%w: %v", apperr.ErrSync, err)
	}
	rc, _, err := be.Download(ctx, rf.key)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(dir, backup.SyncTempPrefix+"*.tmp")
	if err != nil {
		return fmt.Errorf("%w: %v", apperr.ErrSync, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	s256, s1 := sha256.New(), sha1.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, s256, s1), rc)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("%w: download %s: %v", apperr.ErrSync, rf.key, errors.Join(copyErr, closeErr))
	}
	got := hex.EncodeToString(s1.Sum(nil))
	if rf.obj.SHA1 != "" && len(rf.obj.SHA1) == 40 && !strings.EqualFold(got, rf.obj.SHA1) {
		return fmt.Errorf("%w: download %s: content does not match its SHA-1", apperr.ErrSync, rf.key)
	}
	// Abort if the local file changed (or appeared) since the decision.
	cur, statErr := os.Stat(p)
	switch {
	case seen == nil && statErr == nil:
		return fmt.Errorf("%w: %s appeared during sync; will retry next run", apperr.ErrSync, p)
	case seen != nil && (statErr != nil || cur.Size() != seen.Size() || !cur.ModTime().Equal(seen.ModTime())):
		return fmt.Errorf("%w: %s changed during sync; will retry next run", apperr.ErrSync, p)
	}
	rt := remoteTime(rf.obj)
	if !rt.IsZero() {
		if err := os.Chtimes(tmpName, rt, rt); err != nil {
			return fmt.Errorf("%w: %v", apperr.ErrSync, err)
		}
	}
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("%w: %v", apperr.ErrSync, err)
	}
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("%w: %v", apperr.ErrSync, err)
	}
	return st.UpsertFile(ctx, recordFor(p, info, hashes{sha256: hex.EncodeToString(s256.Sum(nil)), sha1: got},
		rf.key, rf.obj, rt, now))
}

// setAside moves a local file that lost a conflict to dest (creating its
// folder), copying when a rename across filesystems isn't possible. It never
// overwrites: an existing dest gets " (2)", " (3)", ... before its extension.
// (Only one bb process syncs at a time — see Lock — so the free-name check
// can't race another sync.)
func setAside(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	dest, err := freeName(dest)
	if err != nil {
		return err
	}
	if err := os.Rename(src, dest); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		in.Close()
		return err
	}
	_, copyErr := io.Copy(out, in)
	in.Close()
	if err := errors.Join(copyErr, out.Close()); err != nil {
		os.Remove(dest)
		return err
	}
	return os.Remove(src)
}

// maxFreeNameTries bounds freeName's search, so an odd filesystem state can
// never hang a sync (the daemon would hold its lock forever).
const maxFreeNameTries = 1000

// maxNameBytes keeps a candidate's file name under the common 255-byte limit,
// leaving room for the " (n)" suffix.
const maxNameBytes = 240

// freeName returns p, or p with " (n)" before its extension for the first n
// that names no existing file. Any stat error other than "doesn't exist"
// (e.g. a name or path that is too long) fails instead of looping, and an
// over-long name is shortened before numbering.
func freeName(p string) (string, error) {
	_, err := os.Lstat(p)
	switch {
	case os.IsNotExist(err):
		return p, nil
	case err != nil && !errors.Is(err, syscall.ENAMETOOLONG):
		return "", err
	}
	dir, base := filepath.Split(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if len(ext) > maxNameBytes/2 {
		ext = ""
		stem = base
	}
	for len(stem)+len(ext) > maxNameBytes {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	for n := 2; n < maxFreeNameTries; n++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
		_, err := os.Lstat(c)
		if os.IsNotExist(err) {
			return c, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("%w: no free name for %s", apperr.ErrSync, p)
}

// hashes are a local file's content hashes: SHA-256 (bb's change detection)
// and SHA-1 (what B2 records per version).
type hashes struct{ sha256, sha1 string }

// hashFile reads p once and returns both hashes.
func hashFile(p string) (hashes, error) {
	f, err := os.Open(p)
	if err != nil {
		return hashes{}, err
	}
	defer f.Close()
	s256, s1 := sha256.New(), sha1.New()
	if _, err := io.Copy(io.MultiWriter(s256, s1), f); err != nil {
		return hashes{}, err
	}
	return hashes{sha256: hex.EncodeToString(s256.Sum(nil)), sha1: hex.EncodeToString(s1.Sum(nil))}, nil
}
