package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
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
	// Meta records, per key, what the last Upload/UploadWithModTime stored: a new
	// unique FileID on every upload ("fake-1", "fake-2", ...), the source
	// modification time and the upload time. List reports them; Delete drops them.
	Meta map[string]FakeMeta
	// Uploads and Downloads log the keys of successful Upload/UploadWithModTime
	// and Download calls, in call order.
	Uploads   []string
	Downloads []string

	nextID int // last FileID number handed out
}

// FakeMeta is FakeBackend's per-key record of the last upload (see FakeBackend.Meta).
type FakeMeta struct {
	FileID      string
	SrcModified time.Time
	Uploaded    time.Time
	// sha1 is the hex SHA-1 of the uploaded bytes. When it no longer matches
	// Objects[key] (a test assigned the map directly), List ignores this Meta.
	// Empty (a hand-written FakeMeta) means always honour it.
	sha1 string
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
	return fakeSHA1(data), true, nil
}

// NewFake returns an empty in-memory backend.
func NewFake() *FakeBackend {
	return &FakeBackend{Objects: map[string][]byte{}}
}

// Upload reads r fully and stores the bytes in memory under key.
func (f *FakeBackend) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	return f.UploadWithModTime(ctx, key, r, size, time.Time{})
}

// UploadWithModTime is Upload that also records modTime as the key's source
// modification time (see b2.ModTimeUploader); every call assigns a new FileID.
func (f *FakeBackend) UploadWithModTime(ctx context.Context, key string, r io.Reader, size int64, modTime time.Time) error {
	if f.FailUpload != nil {
		if err := f.FailUpload(key); err != nil {
			return err
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if f.Objects == nil {
		f.Objects = map[string][]byte{}
	}
	if f.Meta == nil {
		f.Meta = map[string]FakeMeta{}
	}
	f.Objects[key] = data
	f.nextID++
	f.Meta[key] = FakeMeta{
		FileID:      fmt.Sprintf("fake-%d", f.nextID),
		SrcModified: modTime,
		Uploaded:    time.Now(),
		sha1:        fakeSHA1(data),
	}
	f.Uploads = append(f.Uploads, key)
	return nil
}

// fakeSHA1 returns the hex SHA-1 of data.
func fakeSHA1(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// objectInfo describes key as List reports it: SHA1 always, and FileID,
// SrcModified and Modified from Meta when it still describes the stored bytes.
// Otherwise (bytes assigned directly) the FileID is derived from key + content,
// so reassigning different bytes changes it.
func (f *FakeBackend) objectInfo(key string) ObjectInfo {
	data := f.Objects[key]
	sum := fakeSHA1(data)
	info := ObjectInfo{Key: key, Size: int64(len(data)), SHA1: sum}
	if m, ok := f.Meta[key]; ok && (m.sha1 == "" || m.sha1 == sum) {
		info.FileID = m.FileID
		info.SrcModified = m.SrcModified
		info.Modified = m.Uploaded
	}
	if info.FileID == "" {
		id := sha1.Sum([]byte(key + "\x00" + sum))
		info.FileID = "fake-direct-" + hex.EncodeToString(id[:8])
	}
	return info
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
	f.Downloads = append(f.Downloads, key)
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// Delete removes key from memory, or returns apperr.ErrObjectNotFound if absent.
func (f *FakeBackend) Delete(ctx context.Context, key string) error {
	if _, ok := f.Objects[key]; !ok {
		return apperr.ErrObjectNotFound
	}
	delete(f.Objects, key)
	delete(f.Meta, key)
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
			out.Objects = append(out.Objects, f.objectInfo(k))
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
		out.Objects = append(out.Objects, f.objectInfo(k))
	}
	return out, nil
}
