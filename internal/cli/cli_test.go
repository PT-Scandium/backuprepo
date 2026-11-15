package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
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
	// With a lister that finds "my-bucket", Init auto-resolves the bucket ID and
	// does NOT prompt for it, so no bucket-ID line appears in the input.
	lister := func(context.Context, b2.Config) ([]b2.BucketInfo, error) {
		return []b2.BucketInfo{{Name: "my-bucket", ID: "auto-bid", Type: "allPrivate"}}, nil
	}
	in := strings.NewReader(strings.Join([]string{
		"0001keyid",                              // keyID
		"K001-this-is-secret",                    // appKey
		"my-bucket",                              // bucket name (ID auto-resolved)
		"https://s3.us-west-004.backblazeb2.com", // endpoint
		"us-west-004",                            // region
		"",                                       // first folder (skip)
	}, "\n"))
	var out bytes.Buffer
	if err := Init(ctx, st, in, &out, lister); err != nil {
		t.Fatalf("Init: %v", err)
	}

	cfg, err := st.GetConfig(ctx)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if cfg.BucketID != "auto-bid" {
		t.Fatalf("bucket ID not auto-resolved: got %q, want %q", cfg.BucketID, "auto-bid")
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

// TestInitBucketIDFallback verifies that when auto-resolution fails (lister
// errors) or the bucket isn't found, Init falls back to the manual ID prompt.
func TestInitBucketIDFallback(t *testing.T) {
	cases := []struct {
		name   string
		lister BucketLister
	}{
		{"lister errors", func(context.Context, b2.Config) ([]b2.BucketInfo, error) {
			return nil, apperr.ErrAuthFailed
		}},
		{"bucket not found", func(context.Context, b2.Config) ([]b2.BucketInfo, error) {
			return []b2.BucketInfo{{Name: "someone-elses-bucket", ID: "x"}}, nil
		}},
		{"nil lister", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			ctx := context.Background()
			in := strings.NewReader(strings.Join([]string{
				"0001keyid",           // keyID
				"K001-this-is-secret", // appKey
				"my-bucket",           // bucket name
				"manual-bid",          // bucket ID (prompted because auto-resolve failed)
				"https://s3.example",  // endpoint
				"us-west-004",         // region
				"",                    // first folder (skip)
			}, "\n"))
			var out bytes.Buffer
			if err := Init(ctx, st, in, &out, tc.lister); err != nil {
				t.Fatalf("Init: %v", err)
			}
			cfg, err := st.GetConfig(ctx)
			if err != nil {
				t.Fatalf("GetConfig: %v", err)
			}
			if cfg.BucketID != "manual-bid" {
				t.Fatalf("bucket ID = %q, want manual-bid (fallback prompt)", cfg.BucketID)
			}
		})
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
	// Explicit id is used as-is; lister must not be consulted, so pass nil.
	if err := Bucket(ctx, st, "new-bucket", "new-id", nil, &out); err != nil {
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
	if err := Bucket(ctx, st, "", "", nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "new-bucket") || !strings.Contains(out.String(), "new-id") {
		t.Fatalf("show missing bucket/id: %q", out.String())
	}
}

// TestBucketSetRequiresConfig verifies setting a bucket before config returns ErrNotConfigured.
func TestBucketSetRequiresConfig(t *testing.T) {
	st := newStore(t)
	err := Bucket(context.Background(), st, "b", "i", nil, &bytes.Buffer{})
	if !errors.Is(err, apperr.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

// TestBucketAutoResolvesID verifies that switching by name only auto-resolves the
// bucket ID from the account, and falls back to clearing it when not found.
func TestBucketAutoResolvesID(t *testing.T) {
	newCfg := func(t *testing.T) (*store.Store, context.Context) {
		st := newStore(t)
		ctx := context.Background()
		if err := st.SaveConfig(ctx, store.RemoteConfig{
			KeyID: "k", AppKey: "secret", Bucket: "old", BucketID: "old-id",
		}); err != nil {
			t.Fatal(err)
		}
		return st, ctx
	}

	t.Run("resolved", func(t *testing.T) {
		st, ctx := newCfg(t)
		lister := func(context.Context, b2.Config) ([]b2.BucketInfo, error) {
			return []b2.BucketInfo{{Name: "new", ID: "resolved-id"}}, nil
		}
		if err := Bucket(ctx, st, "new", "", lister, &bytes.Buffer{}); err != nil {
			t.Fatalf("Bucket: %v", err)
		}
		cfg, _ := st.GetConfig(ctx)
		if cfg.Bucket != "new" || cfg.BucketID != "resolved-id" {
			t.Fatalf("got %s / %s, want new / resolved-id", cfg.Bucket, cfg.BucketID)
		}
	})

	t.Run("not found clears id", func(t *testing.T) {
		st, ctx := newCfg(t)
		lister := func(context.Context, b2.Config) ([]b2.BucketInfo, error) {
			return []b2.BucketInfo{{Name: "someone-else", ID: "x"}}, nil
		}
		if err := Bucket(ctx, st, "new", "", lister, &bytes.Buffer{}); err != nil {
			t.Fatalf("Bucket: %v", err)
		}
		cfg, _ := st.GetConfig(ctx)
		if cfg.Bucket != "new" || cfg.BucketID != "" {
			t.Fatalf("got %s / %q, want new / empty", cfg.Bucket, cfg.BucketID)
		}
	})
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

// TestInitNoInput is the regression for running `bb init` without an
// interactive stdin (e.g. `! bb init` in an agent shell): every prompt read
// EOF and the user was told the fields were "required". Now it says why.
func TestInitNoInput(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := Init(ctx, st, strings.NewReader(""), &out, nil); !errors.Is(err, apperr.ErrNoInput) {
		t.Fatalf("want ErrNoInput, got %v", err)
	}
	if ok, _ := st.IsConfigured(ctx); ok {
		t.Error("nothing may be saved without input")
	}
}

// TestInitBlankRequiredWithInput: blank answers on a live input stream are a
// user error (required fields), not a missing terminal.
func TestInitBlankRequiredWithInput(t *testing.T) {
	st := newStore(t)
	err := Init(context.Background(), st, strings.NewReader("\n\n\nmore\n"), &bytes.Buffer{}, nil)
	if !errors.Is(err, apperr.ErrInvalidCredentials) || errors.Is(err, apperr.ErrNoInput) {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
}

// initAnswers runs Init with the given endpoint/region answers; the bucket ID
// is auto-resolved when resolveID is set, else the bucket-ID prompt gets bid.
func initAnswers(t *testing.T, resolveID bool, bid, endpoint, region string) (*store.Store, string, error) {
	t.Helper()
	st := newStore(t)
	var lister BucketLister
	lines := []string{"0001keyid", "K001-secret", "my-bucket"}
	if resolveID {
		lister = func(context.Context, b2.Config) ([]b2.BucketInfo, error) {
			return []b2.BucketInfo{{Name: "my-bucket", ID: "auto-bid"}}, nil
		}
	} else {
		lines = append(lines, bid)
	}
	lines = append(lines, endpoint, region, "")
	var out bytes.Buffer
	err := Init(context.Background(), st, strings.NewReader(strings.Join(lines, "\n")), &out, lister)
	return st, out.String(), err
}

// TestInitBlankEndpointUsesNativeB2 is the regression for a config saved with
// an empty endpoint/region on the default s3 backend, which then failed every
// upload ("A region must be set"). With a bucket ID, init now picks b2.
func TestInitBlankEndpointUsesNativeB2(t *testing.T) {
	st, out, err := initAnswers(t, true, "", "", "")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if kind, _ := st.GetBackend(context.Background()); kind != "b2" {
		t.Errorf("backend = %q, want b2", kind)
	}
	if !strings.Contains(out, "using the native B2 backend") || !strings.Contains(out, "Configuration saved.") {
		t.Errorf("output = %q", out)
	}
}

func TestInitBlankEndpointWithoutBucketIDFails(t *testing.T) {
	st, _, err := initAnswers(t, false, "", "", "")
	if !errors.Is(err, apperr.ErrInvalidCredentials) || !strings.Contains(err.Error(), "bucket ID") {
		t.Fatalf("want ErrInvalidCredentials about the bucket ID, got %v", err)
	}
	if ok, _ := st.IsConfigured(context.Background()); ok {
		t.Error("an unusable config must not be saved")
	}
}

// TestInitEndpointHostOnlyDerivesRegion: pasting the host from Backblaze's
// Buckets page (no scheme) and leaving region blank yields a working s3 config.
func TestInitEndpointHostOnlyDerivesRegion(t *testing.T) {
	st, out, err := initAnswers(t, true, "", "s3.us-west-004.backblazeb2.com", "")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, _ := st.GetConfig(context.Background())
	if cfg.Endpoint != "https://s3.us-west-004.backblazeb2.com" || cfg.Region != "us-west-004" {
		t.Errorf("endpoint/region = %q/%q", cfg.Endpoint, cfg.Region)
	}
	if kind, _ := st.GetBackend(context.Background()); kind != "s3" {
		t.Errorf("backend = %q, want s3 (default kept)", kind)
	}
	if !strings.Contains(out, "Region: us-west-004 (from endpoint)") {
		t.Errorf("output = %q", out)
	}
}

// TestInitUnderivableRegionFallsBackToB2: a non-Backblaze endpoint with no
// region can't be used by s3, so init falls back to b2 rather than saving a
// broken s3 config.
func TestInitUnderivableRegionFallsBackToB2(t *testing.T) {
	st, out, err := initAnswers(t, false, "bid-1", "https://s3.example.com", "")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if kind, _ := st.GetBackend(context.Background()); kind != "b2" {
		t.Errorf("backend = %q, want b2", kind)
	}
	// The user did give an endpoint: the message must not claim otherwise.
	if !strings.Contains(out, "none could be derived from https://s3.example.com") ||
		strings.Contains(out, "No S3 endpoint given") {
		t.Errorf("output = %q", out)
	}
}

// reinit runs Init again on st with S3 values and the given bucket-ID answer
// (no lister, so the bucket-ID prompt is used).
func reinit(t *testing.T, st *store.Store, bid string) string {
	t.Helper()
	lines := []string{"0001keyid", "K001-secret", "my-bucket", bid,
		"https://s3.us-west-004.backblazeb2.com", "us-west-004", ""}
	var out bytes.Buffer
	if err := Init(context.Background(), st, strings.NewReader(strings.Join(lines, "\n")), &out, nil); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	return out.String()
}

// TestReinitWithS3ValuesNeverLeavesUnusableB2 is the regression for review
// finding S1: SaveConfig keeps the backend column, so re-running init with S3
// values but no bucket ID used to leave backend b2 with an empty bucket ID.
func TestReinitWithS3ValuesNeverLeavesUnusableB2(t *testing.T) {
	ctx := context.Background()
	st, _, err := initAnswers(t, true, "", "", "") // first init: auto-selects b2
	if err != nil {
		t.Fatal(err)
	}
	out := reinit(t, st, "")
	cfg, _ := st.GetConfig(ctx)
	kind, _ := st.GetBackend(ctx)
	if kind != "s3" || cfg.BucketID != "" {
		t.Fatalf("backend=%q bucketID=%q, want s3 with no bucket ID", kind, cfg.BucketID)
	}
	if !strings.Contains(out, "switched to the S3 backend") {
		t.Errorf("switch must be announced: %q", out)
	}
}

// TestReinitKeepsWorkingB2: a deliberate b2 choice that still has a bucket ID
// is left alone when init is re-run with S3 values too.
func TestReinitKeepsWorkingB2(t *testing.T) {
	ctx := context.Background()
	st, _, err := initAnswers(t, true, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	out := reinit(t, st, "bid-9")
	if kind, _ := st.GetBackend(ctx); kind != "b2" {
		t.Errorf("backend = %q, want b2 kept", kind)
	}
	if strings.Contains(out, "switched") {
		t.Errorf("no switch expected: %q", out)
	}
}

func TestNormalizeEndpointAndRegion(t *testing.T) {
	norm := map[string]string{
		"":                                       "",
		"  s3.eu-central-003.backblazeb2.com/  ": "https://s3.eu-central-003.backblazeb2.com",
		"s3.us-west-004.backblazeb2.com/my-bucket/":    "https://s3.us-west-004.backblazeb2.com",
		"https://s3.us-west-004.backblazeb2.com:443/x": "https://s3.us-west-004.backblazeb2.com:443",
		"https://s3.us-west-004.backblazeb2.com":       "https://s3.us-west-004.backblazeb2.com",
		"http://localhost:9000":                        "http://localhost:9000",
	}
	for in, want := range norm {
		if got := normalizeEndpoint(in); got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
	region := map[string]string{
		"https://s3.us-west-004.backblazeb2.com":        "us-west-004",
		"https://S3.US-WEST-004.BACKBLAZEB2.COM":        "us-west-004",
		"https://s3.eu-central-003.backblazeb2.com:443": "eu-central-003",
		"https://s3.example.com":                        "",
		"https://backblazeb2.com":                       "",
		"https://s3..backblazeb2.com":                   "",
		"://bad":                                        "",
	}
	for in, want := range region {
		if got := regionFromEndpoint(in); got != want {
			t.Errorf("regionFromEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
