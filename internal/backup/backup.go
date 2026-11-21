// Package backup walks watched folders, detects changed files against the
// store, and uploads them via the b2.Uploader. Change detection uses size+mtime
// first and falls back to a SHA-256 content hash, skipping unchanged files.
package backup

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
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/store"
)

// Service orchestrates change detection and uploads.
type Service struct {
	store *store.Store
	up    b2.Uploader
	del   b2.Deleter // optional; nil disables deletion propagation (the default)
}

// New constructs a Service from a store and an uploader. Deletion propagation is
// off by default — see WithDeleter.
func New(st *store.Store, up b2.Uploader) *Service {
	return &Service{store: st, up: up}
}

// WithDeleter enables deletion propagation: after uploading, UploadChanged will
// remove remote objects (and local records) for tracked files that no longer
// exist on disk. Opt-in because it is destructive. Returns s for chaining.
func (s *Service) WithDeleter(del b2.Deleter) *Service {
	s.del = del
	return s
}

// Result summarizes an UploadChanged run.
type Result struct {
	Uploaded int
	Skipped  int
	Adopted  int // new files the bucket already held byte-for-byte (recorded, not uploaded; ADR-027)
	// HashLookupsFailed counts files uploaded because asking the server for
	// their SHA-1 failed: possibly a needless new version, never a missed backup.
	HashLookupsFailed int
	Deleted           int
	Failed            int
	Errors            []error
}

// UploadChanged scans every watched folder and uploads changed/new files.
// A single file's failure is recorded but does not abort the run.
func (s *Service) UploadChanged(ctx context.Context) (Result, error) {
	var res Result
	err := s.eachFile(ctx, func(path string, info fs.FileInfo, key string) error {
		changed, hash, prior, err := s.isChanged(ctx, path, info, key)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, err)
			return nil
		}
		if !changed {
			res.Skipped++
			return nil
		}
		if !backedUpUnder(prior, key) {
			adopted, lookupErr := s.adopt(ctx, path, info, hash, key)
			if adopted {
				res.Adopted++
				return nil
			}
			if lookupErr {
				res.HashLookupsFailed++
			}
		}
		if err := s.uploadOne(ctx, path, info, hash, key); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, err)
			return nil
		}
		res.Uploaded++
		return nil
	})
	if err != nil {
		return res, err
	}
	if s.del != nil {
		if err := s.propagateDeletions(ctx, &res); err != nil {
			return res, err
		}
	}
	if res.Failed > 0 {
		return res, fmt.Errorf("%w: %d file(s) failed", apperr.ErrUploadFailed, res.Failed)
	}
	return res, nil
}

// propagateDeletions removes remote objects (and their local records) for tracked
// files that no longer exist on disk but still live under a watched folder. It is
// only invoked when a deleter is configured (WithDeleter).
func (s *Service) propagateDeletions(ctx context.Context, res *Result) error {
	folders, err := s.store.ListFolders(ctx)
	if err != nil {
		return err
	}
	// Only consider folders that currently exist as directories. If a watched
	// folder is missing (e.g. an unmounted drive), skip deletions under it rather
	// than purging every backup it ever held — a transient mount failure must not
	// be mistaken for "the user deleted everything."
	var live []string
	for _, f := range folders {
		if info, statErr := os.Stat(f); statErr == nil && info.IsDir() {
			live = append(live, f)
		}
	}
	if len(live) == 0 {
		return nil
	}
	files, err := s.store.ListFiles(ctx)
	if err != nil {
		return err
	}
	for _, fr := range files {
		if fr.RemoteKey == "" || !underAny(live, fr.Path) {
			continue
		}
		if _, statErr := os.Lstat(fr.Path); statErr == nil {
			continue // still present locally
		} else if !os.IsNotExist(statErr) {
			continue // uncertain (permission, I/O error) — never delete on doubt
		}
		// The file is gone. Delete the remote object first; treat already-absent
		// as success so a retried run still clears the local record.
		if err := s.del.Delete(ctx, fr.RemoteKey); err != nil && !errors.Is(err, apperr.ErrObjectNotFound) {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Errorf("%w: delete remote for %s: %v", apperr.ErrDeleteFailed, fr.Path, err))
			continue
		}
		if err := s.store.RemoveFile(ctx, fr.Path); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, err)
			continue
		}
		res.Deleted++
	}
	return nil
}

// underAny reports whether path lies within any of the given folders.
func underAny(folders []string, path string) bool {
	for _, f := range folders {
		if rel, err := filepath.Rel(f, path); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// PendingCount returns how many files would be uploaded by UploadChanged. A new
// file the bucket already holds (adopted without an upload) is still counted:
// telling the two apart needs a request to the server per file.
func (s *Service) PendingCount(ctx context.Context) (int, error) {
	n := 0
	err := s.eachFile(ctx, func(path string, info fs.FileInfo, key string) error {
		changed, _, _, err := s.isChanged(ctx, path, info, key)
		if err != nil {
			return err
		}
		if changed {
			n++
		}
		return nil
	})
	return n, err
}

// eachFile walks all watched folders and calls fn for each regular file with
// the object key it is stored under (see KeyFor). A watched folder nested in
// another is skipped by the outer walk and walked on its own, so each file is
// visited once.
func (s *Service) eachFile(ctx context.Context, fn func(path string, info fs.FileInfo, key string) error) error {
	folders, err := s.store.ListWatched(ctx)
	if err != nil {
		return err
	}
	for _, folder := range folders {
		root := filepath.Clean(folder.Path)
		walkErr := filepath.WalkDir(folder.Path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries rather than abort the whole walk
			}
			if d.IsDir() {
				if !samePath(filepath.Clean(path), root) && isWatchedRoot(folders, path) {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			return fn(path, info, KeyFor(folders, path))
		})
		if walkErr != nil {
			return walkErr
		}
	}
	return nil
}

// isChanged reports whether path differs from its stored record or was backed
// up under a key other than key (its folder's bucket prefix changed). It
// returns the content hash (computed only when needed) and the prior record
// (may be nil).
func (s *Service) isChanged(ctx context.Context, path string, info fs.FileInfo, key string) (bool, string, *store.FileRecord, error) {
	prior, err := s.store.GetFile(ctx, path)
	if err != nil {
		return false, "", nil, err
	}
	size := info.Size()
	mtime := info.ModTime().Unix()
	backedUp := backedUpUnder(prior, key)
	if backedUp && prior.Size == size && prior.ModTime == mtime {
		return false, prior.SHA256, prior, nil // cheap check: unchanged
	}
	hash, err := hashFile(path)
	if err != nil {
		return false, "", prior, fmt.Errorf("%w: hash %s: %v", apperr.ErrUploadFailed, path, err)
	}
	if backedUp && prior.SHA256 == hash {
		// content identical (only metadata moved); refresh metadata, no upload
		_ = s.store.UpsertFile(ctx, store.FileRecord{
			Path: path, Size: size, ModTime: mtime, SHA256: hash,
			LastBackup: prior.LastBackup, RemoteKey: key,
		})
		return false, hash, prior, nil
	}
	return true, hash, prior, nil
}

// backedUpUnder reports whether prior records a completed backup stored under
// key. A record without a stored key was written before keys were recorded,
// when every object had its default name, so it matches only that name.
func backedUpUnder(prior *store.FileRecord, key string) bool {
	if prior == nil || prior.LastBackup == nil {
		return false
	}
	return storedKey(*prior) == key
}

// storedKey is the object key a record was backed up under: its stored key, or
// the default name for a record from before keys were recorded.
func storedKey(fr store.FileRecord) string {
	if fr.RemoteKey == "" {
		return RemoteKey(fr.Path)
	}
	return fr.RemoteKey
}

// adopt records path as backed up without uploading it when the bucket already
// holds key with the file's exact content (same SHA-1), e.g. after
// `bb get -r` into a watched folder whose prefix maps back to where the files
// came from. It reports false, so the caller uploads, whenever the backend
// can't report a hash, the object is missing, the hashes differ, or any step
// fails: uploading is always the safe fallback (ADR-027). lookupFailed reports
// that the server couldn't be asked, so callers can surface it.
func (s *Service) adopt(ctx context.Context, path string, info fs.FileInfo, hash, key string) (adopted, lookupFailed bool) {
	h, ok := s.up.(b2.ContentHasher)
	if !ok {
		return false, false
	}
	remote, found, err := h.RemoteSHA1(ctx, key)
	if err != nil {
		return false, true
	}
	if !found || len(remote) != sha1.Size*2 {
		return false, false
	}
	local, err := sha1File(path)
	if err != nil || !strings.EqualFold(local, remote) {
		return false, false
	}
	now := time.Now().Unix()
	return s.store.UpsertFile(ctx, store.FileRecord{
		Path: path, Size: info.Size(), ModTime: info.ModTime().Unix(),
		SHA256: hash, LastBackup: &now, RemoteKey: key,
	}) == nil, false
}

// uploadOne uploads the file to key and records its new backup state.
func (s *Service) uploadOne(ctx context.Context, path string, info fs.FileInfo, hash, key string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open %s: %v", apperr.ErrUploadFailed, path, err)
	}
	defer f.Close()
	if err := s.up.Upload(ctx, key, f, info.Size()); err != nil {
		return err
	}
	now := time.Now().Unix()
	return s.store.UpsertFile(ctx, store.FileRecord{
		Path: path, Size: info.Size(), ModTime: info.ModTime().Unix(),
		SHA256: hash, LastBackup: &now, RemoteKey: key,
	})
}

// RemoteKey maps an absolute local path to its default bucket object key: the
// path without its leading "/" (ADR-003). Files in a folder watched with a
// bucket prefix use KeyFor instead.
func RemoteKey(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), "/")
}

// KeyFor returns the object key for the file at path. The innermost watched
// folder containing path decides: with a bucket prefix the key is that prefix
// plus path relative to the folder (ADR-027); without one (or outside every
// folder) it is RemoteKey(path), the name bb has always used.
func KeyFor(folders []store.Folder, path string) string {
	best, bestLen, rel := -1, -1, ""
	for i, f := range folders {
		root := filepath.Clean(f.Path)
		r, err := filepath.Rel(root, path)
		if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			continue
		}
		if len(root) > bestLen {
			best, bestLen, rel = i, len(root), r
		}
	}
	if best < 0 || folders[best].RemotePrefix == "" {
		return RemoteKey(path)
	}
	return folders[best].RemotePrefix + filepath.ToSlash(rel)
}

// isWatchedRoot reports whether dir is itself one of the watched folders,
// comparing absolute forms (so a legacy relative entry matches too) and
// ignoring case on Windows, as KeyFor's filepath.Rel does there.
func isWatchedRoot(folders []store.Folder, dir string) bool {
	d, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for _, f := range folders {
		if p, err := filepath.Abs(f.Path); err == nil && samePath(p, d) {
			return true
		}
	}
	return false
}

// samePath compares two cleaned paths as the OS does: case-insensitively on
// Windows (NTFS default), exactly elsewhere.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// sha1File returns the hex-encoded SHA-1 of the file at path, the hash B2
// records for each upload (used only to compare with it, never for integrity).
func sha1File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashFile returns the hex-encoded SHA-256 of the file at path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
