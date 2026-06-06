// Package b2 uploads objects to a Backblaze B2 bucket via the S3-compatible API.
// Uploader is the abstraction backup logic depends on; FakeUploader backs tests.
package b2

import (
	"context"
	"io"
)

// Uploader stores objects in the destination bucket.
type Uploader interface {
	Upload(ctx context.Context, key string, r io.Reader, size int64) error
	Exists(ctx context.Context, key string) (bool, error)
}

// Config is the destination credentials/addressing for the real uploader.
type Config struct {
	Endpoint string
	Region   string
	Bucket   string
	KeyID    string
	AppKey   string
}

// FakeUploader is an in-memory Uploader for tests.
type FakeUploader struct {
	Objects map[string][]byte
}

// NewFake returns an empty in-memory uploader.
func NewFake() *FakeUploader {
	return &FakeUploader{Objects: map[string][]byte{}}
}

// Upload reads r fully and stores it under key.
func (f *FakeUploader) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.Objects[key] = data
	return nil
}

// Exists reports whether key was previously uploaded.
func (f *FakeUploader) Exists(ctx context.Context, key string) (bool, error) {
	_, ok := f.Objects[key]
	return ok, nil
}
