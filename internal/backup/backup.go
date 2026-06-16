// Package backup walks watched folders, detects changed files against the
// store, and uploads them via the b2.Uploader. Change detection uses size+mtime
// first and falls back to a SHA-256 content hash, skipping unchanged files.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	Deleted  int
	Failed   int
	Errors   []error
}

// UploadChanged scans every watched folder and uploads changed/new files.
// A single file's failure is recorded but does not abort the run.
func (s *Service) UploadChanged(ctx context.Context) (Result, error) {
	var res Result
	err := s.eachFile(ctx, func(path string, info fs.FileInfo) error {
		changed, hash, prior, err := s.isChanged(ctx, path, info)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, err)
			return nil
		}
		if !changed {
			res.Skipped++
			return nil
		}
		if err := s.uploadOne(ctx, path, info, hash, prior); err != nil {
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

// PendingCount returns how many files would be uploaded by UploadChanged.
func (s *Service) PendingCount(ctx context.Context) (int, error) {
	n := 0
	err := s.eachFile(ctx, func(path string, info fs.FileInfo) error {
		changed, _, _, err := s.isChanged(ctx, path, info)
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

// eachFile walks all watched folders and calls fn for each regular file.
func (s *Service) eachFile(ctx context.Context, fn func(path string, info fs.FileInfo) error) error {
	folders, err := s.store.ListFolders(ctx)
	if err != nil {
		return err
	}
	for _, folder := range folders {
		walkErr := filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries rather than abort the whole walk
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			return fn(path, info)
		})
		if walkErr != nil {
			return walkErr
		}
	}
	return nil
}

// isChanged reports whether path differs from its stored record. It returns the
// content hash (computed only when needed) and the prior record (may be nil).
func (s *Service) isChanged(ctx context.Context, path string, info fs.FileInfo) (bool, string, *store.FileRecord, error) {
	prior, err := s.store.GetFile(ctx, path)
	if err != nil {
		return false, "", nil, err
	}
	size := info.Size()
	mtime := info.ModTime().Unix()
	if prior != nil && prior.Size == size && prior.ModTime == mtime && prior.LastBackup != nil {
		return false, prior.SHA256, prior, nil // cheap check: unchanged
	}
	hash, err := hashFile(path)
	if err != nil {
		return false, "", prior, fmt.Errorf("%w: hash %s: %v", apperr.ErrUploadFailed, path, err)
	}
	if prior != nil && prior.SHA256 == hash && prior.LastBackup != nil {
		// content identical (only metadata moved); refresh metadata, no upload
		_ = s.store.UpsertFile(ctx, store.FileRecord{
			Path: path, Size: size, ModTime: mtime, SHA256: hash,
			LastBackup: prior.LastBackup, RemoteKey: prior.RemoteKey,
		})
		return false, hash, prior, nil
	}
	return true, hash, prior, nil
}

// uploadOne uploads the file and records its new backup state.
func (s *Service) uploadOne(ctx context.Context, path string, info fs.FileInfo, hash string, prior *store.FileRecord) error {
	key := RemoteKey(path)
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

// RemoteKey maps an absolute local path to a stable bucket object key.
func RemoteKey(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), "/")
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := copyInto(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyInto(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}
