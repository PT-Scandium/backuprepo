package b2

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestFakeUploaderStoresAndReports(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	ok, err := f.Exists(ctx, "k")
	if err != nil || ok {
		t.Fatalf("expected not exist: %v, %v", ok, err)
	}
	if err := f.Upload(ctx, "k", bytes.NewReader([]byte("payload")), 7); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	ok, err = f.Exists(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("expected exist after upload: %v, %v", ok, err)
	}
	if got := string(f.Objects["k"]); got != "payload" {
		t.Fatalf("stored %q", got)
	}
}

// compile-time assertions that both impls satisfy the interface.
var _ Uploader = (*FakeUploader)(nil)
var _ Uploader = (*S3Uploader)(nil)

var _ io.Reader = bytes.NewReader(nil)
