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

// key32 returns a deterministic 32-byte encryption key for tests.
func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// newStore opens a fresh encrypted store in a temp dir for tests.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"), key32())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestInitThenConfigMasksSecret verifies Config never echoes the stored app key.
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

// TestWatchUnwatchList verifies watch/list/unwatch and that unwatching twice errors.
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

// TestWatchRejectsMissingDir verifies watching a nonexistent dir returns ErrFolderNotFound.
func TestWatchRejectsMissingDir(t *testing.T) {
	st := newStore(t)
	var out bytes.Buffer
	err := Watch(context.Background(), st, "/no/such/dir/here", &out)
	if !errors.Is(err, apperr.ErrFolderNotFound) {
		t.Fatalf("want ErrFolderNotFound, got %v", err)
	}
}

// TestStatusNotConfigured verifies Status reports "not configured" before init.
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

// TestBucketSwitchKeepsCredentials verifies switching buckets leaves credentials and endpoint intact.
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

// TestBucketSetRequiresConfig verifies setting a bucket before config returns ErrNotConfigured.
func TestBucketSetRequiresConfig(t *testing.T) {
	st := newStore(t)
	err := Bucket(context.Background(), st, "b", "i", &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

// TestSetAppKeyChangesSecretOnly verifies SetAppKey updates only the secret and never echoes it.
func TestSetAppKeyChangesSecretOnly(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.SaveConfig(ctx, store.RemoteConfig{
		KeyID: "old-id", AppKey: "old-secret", Bucket: "b", BucketID: "bid",
		Endpoint: "e", Region: "r",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	const secret = "K005-NEW-APPLICATION-KEY-VALUE"
	if err := SetAppKey(ctx, st, "", strings.NewReader(secret+"\n"), &out); err != nil {
		t.Fatalf("SetAppKey: %v", err)
	}
	cfg, err := st.GetConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppKey != secret {
		t.Fatalf("appKey not updated: %q", cfg.AppKey)
	}
	// keyID and everything else untouched.
	if cfg.KeyID != "old-id" || cfg.Bucket != "b" || cfg.BucketID != "bid" || cfg.Endpoint != "e" || cfg.Region != "r" {
		t.Fatalf("non-secret fields changed: %+v", cfg)
	}
	// The secret must never be echoed back.
	if strings.Contains(out.String(), secret) {
		t.Fatalf("secret leaked into output: %q", out.String())
	}
}

// TestSetAppKeyRotatesKeyID verifies a non-empty keyID rotates both keyID and secret.
func TestSetAppKeyRotatesKeyID(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.SaveConfig(ctx, store.RemoteConfig{KeyID: "old-id", AppKey: "old", Bucket: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := SetAppKey(ctx, st, "new-id", strings.NewReader("new-secret\n"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := st.GetConfig(ctx)
	if cfg.KeyID != "new-id" || cfg.AppKey != "new-secret" {
		t.Fatalf("pair not rotated: %s / %s", cfg.KeyID, cfg.AppKey)
	}
}

// TestSetAppKeyEmptyRejectedKeepsOld verifies an empty secret is rejected and the old one kept.
func TestSetAppKeyEmptyRejectedKeepsOld(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.SaveConfig(ctx, store.RemoteConfig{KeyID: "old-id", AppKey: "keepme", Bucket: "b"}); err != nil {
		t.Fatal(err)
	}
	err := SetAppKey(ctx, st, "", strings.NewReader("   \n"), &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials for empty secret, got %v", err)
	}
	cfg, _ := st.GetConfig(ctx)
	if cfg.AppKey != "keepme" {
		t.Fatalf("secret should be unchanged after rejection, got %q", cfg.AppKey)
	}
}

// TestSetAppKeyRequiresConfig verifies SetAppKey before config returns ErrNotConfigured.
func TestSetAppKeyRequiresConfig(t *testing.T) {
	st := newStore(t)
	err := SetAppKey(context.Background(), st, "", strings.NewReader("x\n"), &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}
