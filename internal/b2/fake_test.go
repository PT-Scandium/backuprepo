package b2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"backuprepo/internal/apperr"
)

func TestFakeUploadDownloadExists(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	if ok, _ := f.Exists(ctx, "k"); ok {
		t.Fatal("should not exist yet")
	}
	if err := f.Upload(ctx, "k", bytes.NewReader([]byte("payload")), 7); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if ok, _ := f.Exists(ctx, "k"); !ok {
		t.Fatal("should exist after upload")
	}
	rc, n, err := f.Download(ctx, "k")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "payload" || n != 7 {
		t.Fatalf("download mismatch: %q n=%d", data, n)
	}
}

func TestFakeDownloadMissing(t *testing.T) {
	_, _, err := NewFake().Download(context.Background(), "nope")
	if !errors.Is(err, apperr.ErrObjectNotFound) {
		t.Fatalf("want ErrObjectNotFound, got %v", err)
	}
}

func TestFakeListNonRecursiveGroupsFolders(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	for _, k := range []string{"a.txt", "photos/1.jpg", "photos/2.jpg", "photos/sub/3.jpg"} {
		f.Upload(ctx, k, bytes.NewReader([]byte("x")), 1)
	}
	l, err := f.List(ctx, "", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Objects) != 1 || l.Objects[0].Key != "a.txt" {
		t.Fatalf("objects = %+v", l.Objects)
	}
	if len(l.Prefixes) != 1 || l.Prefixes[0] != "photos/" {
		t.Fatalf("prefixes = %+v", l.Prefixes)
	}
}

func TestFakeListRecursive(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	for _, k := range []string{"photos/1.jpg", "photos/sub/3.jpg"} {
		f.Upload(ctx, k, bytes.NewReader([]byte("x")), 1)
	}
	l, err := f.List(ctx, "photos/", true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Objects) != 2 || len(l.Prefixes) != 0 {
		t.Fatalf("recursive list = %+v", l)
	}
}

func TestFakeDelete(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.Upload(ctx, "k", bytes.NewReader([]byte("x")), 1)
	if err := f.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := f.Exists(ctx, "k"); ok {
		t.Fatal("should be gone")
	}
	if err := f.Delete(ctx, "k"); !errors.Is(err, apperr.ErrObjectNotFound) {
		t.Fatalf("want ErrObjectNotFound, got %v", err)
	}
}

// compile-time interface assertions
var _ Backend = (*FakeBackend)(nil)
var _ Backend = (*S3Backend)(nil)
var _ Uploader = (*FakeBackend)(nil)
