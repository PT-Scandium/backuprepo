// Package b2 stores and retrieves objects in a Backblaze B2 bucket, via either
// the S3-compatible API (S3Backend) or the native B2 API (B2Backend). Backend
// is the abstraction manual file commands depend on; Uploader is the narrower
// view the backup flow needs. FakeBackend backs tests.
package b2

import (
	"context"
	"io"
	"time"

	"backuprepo/internal/apperr"
)

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Listing is the result of List: files directly under a prefix, plus "folder"
// prefixes (common prefixes) when not listing recursively.
type Listing struct {
	Objects  []ObjectInfo
	Prefixes []string
}

// BucketInfo describes one bucket in the account. It is not part of the Backend
// interface: listing buckets is an account-level operation supported only by the
// native B2 API (the S3-compatible API cannot return bucket IDs).
type BucketInfo struct {
	Name string
	ID   string
	Type string // B2 bucket type, e.g. "allPrivate" or "allPublic"
}

// ListBuckets lists every bucket the credentials in cfg can see. It always uses
// the native B2 API regardless of the configured backend, because enumerating
// buckets and returning their IDs is a native-only capability.
func ListBuckets(ctx context.Context, cfg Config) ([]BucketInfo, error) {
	return newB2Backend(cfg).ListBuckets(ctx)
}

// Uploader is the narrow write view the backup flow depends on.
type Uploader interface {
	Upload(ctx context.Context, key string, r io.Reader, size int64) error
	Exists(ctx context.Context, key string) (bool, error)
}

// Deleter removes a single object. Backend satisfies it; the backup flow uses it
// only when deletion propagation is explicitly enabled (opt-in, destructive).
type Deleter interface {
	Delete(ctx context.Context, key string) error
}

// Backend is the full bucket interface used by the manual file commands.
type Backend interface {
	Uploader
	Download(ctx context.Context, key string) (io.ReadCloser, int64, error)
	List(ctx context.Context, prefix string, recursive bool) (Listing, error)
	Delete(ctx context.Context, key string) error
}

// Config carries everything both backends need. BucketName is used by the S3
// API and by B2 download-by-name; BucketID is used by the B2 native list/upload.
type Config struct {
	Endpoint   string
	Region     string
	BucketName string
	BucketID   string
	KeyID      string
	AppKey     string
}

// NewBackend builds the backend for kind ("s3" or "b2").
func NewBackend(ctx context.Context, kind string, cfg Config) (Backend, error) {
	switch kind {
	case "", "s3":
		return newS3Backend(ctx, cfg)
	case "b2":
		return newB2Backend(cfg), nil
	default:
		return nil, apperr.ErrInvalidBackend
	}
}
