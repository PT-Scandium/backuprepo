package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"sort"
	"strings"
	"time"

	"backuprepo/internal/apperr"
)

// FakeBackend is an in-memory Backend for tests.
type FakeBackend struct {
	Objects map[string][]byte
	// FailUpload, if set, is consulted before each Upload; a non-nil return makes
	// that key's upload fail (used to exercise error handling).
	FailUpload func(key string) error
	// ReportSHA1 makes RemoteSHA1 report stored objects' SHA-1, so tests can
	// exercise adopting identical remote objects (ADR-027). Off by default: the
	// fake then behaves like a backend without that capability.
	ReportSHA1 bool
	// SHA1Override, when set for a key, is reported instead of the computed hash
	// (e.g. "none", as B2 reports for large files).
	SHA1Override map[string]string
}

// RemoteSHA1 reports the SHA-1 of the stored bytes for key when ReportSHA1 is
// set, and found=false otherwise (see b2.ContentHasher).
func (f *FakeBackend) RemoteSHA1(ctx context.Context, key string) (string, bool, error) {
	data, ok := f.Objects[key]
	if !f.ReportSHA1 || !ok {
		return "", false, nil
	}
	if v, ok := f.SHA1Override[key]; ok {
		return v, true, nil
	}
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:]), true, nil
}

// NewFake returns an empty in-memory backend.
func NewFake() *FakeBackend {
	return &FakeBackend{Objects: map[string][]byte{}}
}

// Upload reads r fully and stores the bytes in memory under key.
func (f *FakeBackend) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	if f.FailUpload != nil {
		if err := f.FailUpload(key); err != nil {
			return err
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.Objects[key] = data
	return nil
}

// Exists reports whether key is present in memory.
func (f *FakeBackend) Exists(ctx context.Context, key string) (bool, error) {
	_, ok := f.Objects[key]
	return ok, nil
}

// Download returns the stored bytes for key, or apperr.ErrObjectNotFound if absent.
func (f *FakeBackend) Download(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	data, ok := f.Objects[key]
	if !ok {
		return nil, 0, apperr.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// Delete removes key from memory, or returns apperr.ErrObjectNotFound if absent.
func (f *FakeBackend) Delete(ctx context.Context, key string) error {
	if _, ok := f.Objects[key]; !ok {
		return apperr.ErrObjectNotFound
	}
	delete(f.Objects, key)
	return nil
}

// List returns objects under prefix; when not recursive, immediate subfolders
// are grouped into Prefixes instead of being listed individually.
func (f *FakeBackend) List(ctx context.Context, prefix string, recursive bool) (Listing, error) {
	var out Listing
	seen := map[string]bool{}
	keys := make([]string, 0, len(f.Objects))
	for k := range f.Objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if recursive {
			out.Objects = append(out.Objects, ObjectInfo{Key: k, Size: int64(len(f.Objects[k])), Modified: time.Time{}})
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			folder := prefix + rest[:i+1]
			if !seen[folder] {
				seen[folder] = true
				out.Prefixes = append(out.Prefixes, folder)
			}
			continue
		}
		out.Objects = append(out.Objects, ObjectInfo{Key: k, Size: int64(len(f.Objects[k])), Modified: time.Time{}})
	}
	return out, nil
}
