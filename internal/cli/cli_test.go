package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"backuprepo/internal/apperr"
	"backuprepo/internal/store"
)

func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"), key32())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestInitThenConfigMasksSecret(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	in := strings.NewReader(strings.Join([]string{
		"0001keyid",                              // keyID
		"K001-this-is-secret",                    // appKey
		"my-bucket",                              // bucket name
		"buck-et-id-123",                         // bucket ID  (NEW)
		"https://s3.us-west-004.backblazeb2.com", // endpoint
		"us-west-004",                            // region
		"",                                       // first folder (skip)
	}, "\n"))
	var out bytes.Buffer
	if err := Init(ctx, st, in, &out); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var cfgOut bytes.Buffer
	if err := Config(ctx, st, &cfgOut); err != nil {
		t.Fatalf("Config: %v", err)
	}
	s := cfgOut.String()
	if strings.Contains(s, "K001-this-is-secret") {
		t.Fatal("config output leaked the secret app key")
	}
	if !strings.Contains(s, "my-bucket") {
		t.Fatalf("config output missing bucket: %q", s)
	}
}

func TestWatchUnwatchList(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	var out bytes.Buffer
	if err := Watch(ctx, st, dir, &out); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	out.Reset()
	if err := List(ctx, st, &out); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !strings.Contains(out.String(), dir) {
		t.Fatalf("List missing folder: %q", out.String())
	}
	if err := Unwatch(ctx, st, dir, &out); err != nil {
		t.Fatalf("Unwatch: %v", err)
	}
	if err := Unwatch(ctx, st, dir, &out); !errors.Is(err, apperr.ErrFolderNotWatched) {
		t.Fatalf("want ErrFolderNotWatched, got %v", err)
	}
}

func TestWatchRejectsMissingDir(t *testing.T) {
	st := newStore(t)
	var out bytes.Buffer
	err := Watch(context.Background(), st, "/no/such/dir/here", &out)
	if !errors.Is(err, apperr.ErrFolderNotFound) {
		t.Fatalf("want ErrFolderNotFound, got %v", err)
	}
}

func TestStatusNotConfigured(t *testing.T) {
	st := newStore(t)
	var out bytes.Buffer
	if err := Status(context.Background(), st, &out); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(out.String(), "not configured") {
		t.Fatalf("status should report not configured: %q", out.String())
	}
}

func TestBucketSwitchKeepsCredentials(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.SaveConfig(ctx, store.RemoteConfig{
		KeyID: "k", AppKey: "secret", Bucket: "old-bucket", BucketID: "old-id",
		Endpoint: "https://e", Region: "r",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Bucket(ctx, st, "new-bucket", "new-id", &out); err != nil {
		t.Fatalf("Bucket set: %v", err)
	}
	cfg, err := st.GetConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bucket != "new-bucket" || cfg.BucketID != "new-id" {
		t.Fatalf("bucket not switched: %s / %s", cfg.Bucket, cfg.BucketID)
	}
	// Credentials, endpoint, region must be untouched.
	if cfg.KeyID != "k" || cfg.AppKey != "secret" || cfg.Endpoint != "https://e" || cfg.Region != "r" {
		t.Fatalf("non-bucket fields changed: %+v", cfg)
	}

	// No-arg form shows the current bucket.
	out.Reset()
	if err := Bucket(ctx, st, "", "", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "new-bucket") || !strings.Contains(out.String(), "new-id") {
		t.Fatalf("show missing bucket/id: %q", out.String())
	}
}

func TestBucketSetRequiresConfig(t *testing.T) {
	st := newStore(t)
	err := Bucket(context.Background(), st, "b", "i", &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}
