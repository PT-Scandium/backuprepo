package b2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

var (
	_ ModTimeUploader = (*B2Backend)(nil)
	_ ModTimeUploader = (*S3Backend)(nil)
	_ ModTimeUploader = (*FakeBackend)(nil)
)

// listObject returns key's ObjectInfo from a recursive List of be.
func listObject(t *testing.T, be Backend, key string) ObjectInfo {
	t.Helper()
	l, err := be.List(context.Background(), "", true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, o := range l.Objects {
		if o.Key == key {
			return o
		}
	}
	t.Fatalf("List: %s not found", key)
	return ObjectInfo{}
}

// TestB2UploadWithModTimeSmall verifies a small upload sends the source
// modification time as X-Bz-Info-src_last_modified_millis, List reports it back
// (also non-recursively), and a later upload without one records nothing.
func TestB2UploadWithModTimeSmall(t *testing.T) {
	store := map[string][]byte{}
	b := testB2(t, b2TestServer(t, store))
	ctx := context.Background()
	mt := time.UnixMilli(1700000000123)

	if err := b.UploadWithModTime(ctx, "a.txt", bytes.NewReader([]byte("v1")), 2, mt); err != nil {
		t.Fatalf("UploadWithModTime: %v", err)
	}
	if got := listObject(t, b, "a.txt").SrcModified; !got.Equal(mt) {
		t.Errorf("SrcModified = %v, want %v", got, mt)
	}
	l, err := b.List(ctx, "", false)
	if err != nil || len(l.Objects) != 1 || !l.Objects[0].SrcModified.Equal(mt) {
		t.Errorf("non-recursive List = %+v, %v", l, err)
	}
	// Upload (zero modTime) must not send the header: the new version has no info.
	if err := b.Upload(ctx, "a.txt", bytes.NewReader([]byte("v2")), 2); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := listObject(t, b, "a.txt").SrcModified; !got.IsZero() {
		t.Errorf("SrcModified after plain Upload = %v, want zero", got)
	}
	if string(store["a.txt"]) != "v2" {
		t.Errorf("stored %q, want v2", store["a.txt"])
	}
}

// TestB2UploadWithModTimeHeaderOnRetry verifies the file info header is sent
// on every attempt, including the retry after a transient failure, and that a
// zero modTime sends no X-Bz-Info-* header at all.
func TestB2UploadWithModTimeHeaderOnRetry(t *testing.T) {
	var calls atomic.Int32
	var headers []string
	var infoHeaders int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/b2api/v3/b2_authorize_account":
			json.NewEncoder(w).Encode(map[string]any{"authorizationToken": "t", "accountId": "a",
				"apiInfo": map[string]any{"storageApi": map[string]any{
					"apiUrl": "http://" + r.Host, "downloadUrl": "http://" + r.Host}}})
		case "/b2api/v3/b2_get_upload_url":
			json.NewEncoder(w).Encode(map[string]any{"uploadUrl": "http://" + r.Host + "/upload", "authorizationToken": "u"})
		case "/upload":
			io.Copy(io.Discard, r.Body)
			headers = append(headers, r.Header.Get("X-Bz-Info-src_last_modified_millis"))
			for h := range r.Header {
				if strings.HasPrefix(h, "X-Bz-Info-") {
					infoHeaders++
				}
			}
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"fileId": "x"})
		}
	}))
	t.Cleanup(srv.Close)
	b := testB2(t, srv)
	ctx := context.Background()
	if err := b.UploadWithModTime(ctx, "k", bytes.NewReader([]byte("x")), 1, time.UnixMilli(42)); err != nil {
		t.Fatalf("UploadWithModTime: %v", err)
	}
	if len(headers) != 2 || headers[0] != "42" || headers[1] != "42" {
		t.Errorf("X-Bz-Info-src_last_modified_millis per attempt = %q, want [42 42]", headers)
	}
	infoHeaders = 0
	if err := b.Upload(ctx, "k", bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if infoHeaders != 0 {
		t.Errorf("zero modTime sent %d X-Bz-Info-* headers, want 0", infoHeaders)
	}
}

// TestB2UploadWithModTimeLarge verifies the large-file path puts the source
// modification time in b2_start_large_file's fileInfo, and that no info
// records nothing.
func TestB2UploadWithModTimeLarge(t *testing.T) {
	store := map[string][]byte{}
	b := testB2(t, b2TestServer(t, store))
	b.partSize = 5
	ctx := context.Background()
	mt := time.UnixMilli(1600000000999)
	payload := []byte("abcdefghijklmnop")

	if err := b.uploadLarge(ctx, mustAuth(t, b, ctx), "big.bin", bytes.NewReader(payload), int64(len(payload)), modTimeMeta(mt)); err != nil {
		t.Fatalf("uploadLarge: %v", err)
	}
	if string(store["big.bin"]) != string(payload) {
		t.Fatalf("reassembled = %q", store["big.bin"])
	}
	if got := listObject(t, b, "big.bin").SrcModified; !got.Equal(mt) {
		t.Errorf("SrcModified = %v, want %v", got, mt)
	}
	if err := b.uploadLarge(ctx, mustAuth(t, b, ctx), "big.bin", bytes.NewReader(payload), int64(len(payload)), modTimeMeta(time.Time{})); err != nil {
		t.Fatalf("uploadLarge (no info): %v", err)
	}
	if got := listObject(t, b, "big.bin").SrcModified; !got.IsZero() {
		t.Errorf("SrcModified without info = %v, want zero", got)
	}
}

// TestModTimeMeta verifies the file info/metadata recording a modification
// time: Unix milliseconds under src_last_modified_millis, nil for zero time,
// and parseable back by srcModified.
func TestModTimeMeta(t *testing.T) {
	if m := modTimeMeta(time.Time{}); m != nil {
		t.Errorf("modTimeMeta(zero) = %v, want nil", m)
	}
	mt := time.Date(2026, 11, 21, 10, 30, 0, 123456789, time.UTC)
	m := modTimeMeta(mt)
	if len(m) != 1 || m["src_last_modified_millis"] != "1795257000123" {
		t.Errorf("modTimeMeta = %v", m)
	}
	if got := srcModified(m); !got.Equal(mt.Truncate(time.Millisecond)) {
		t.Errorf("round trip = %v, want %v", got, mt.Truncate(time.Millisecond))
	}
}

// TestS3PutInputMetadata verifies the S3 PutObject request carries the source
// modification time as user metadata (x-amz-meta-src_last_modified_millis),
// and no metadata for a zero modTime.
func TestS3PutInputMetadata(t *testing.T) {
	r := strings.NewReader("x")
	in := s3PutInput("bkt", "dir/k", r, time.UnixMilli(1700000000123))
	if aws.ToString(in.Bucket) != "bkt" || aws.ToString(in.Key) != "dir/k" || in.Body != r {
		t.Errorf("PutObjectInput = %+v", in)
	}
	if got := in.Metadata["src_last_modified_millis"]; got != "1700000000123" || len(in.Metadata) != 1 {
		t.Errorf("Metadata = %v", in.Metadata)
	}
	if in := s3PutInput("bkt", "k", r, time.Time{}); in.Metadata != nil {
		t.Errorf("zero modTime Metadata = %v, want nil", in.Metadata)
	}
}

// TestFakeUploadWithModTime verifies the fake records a new FileID per upload,
// the source modification time and upload time, and logs uploads.
func TestFakeUploadWithModTime(t *testing.T) {
	ctx := context.Background()
	f := &FakeBackend{} // zero value must work (maps are created lazily)
	mt := time.UnixMilli(1700000000123)
	before := time.Now()
	if err := f.UploadWithModTime(ctx, "a", strings.NewReader("one"), 3, mt); err != nil {
		t.Fatalf("UploadWithModTime: %v", err)
	}
	o1 := listObject(t, f, "a")
	if !o1.SrcModified.Equal(mt) || o1.FileID == "" || o1.Modified.Before(before) {
		t.Errorf("after UploadWithModTime: %+v", o1)
	}
	if o1.SHA1 != "fe05bcdcdc4928012781a5f1a2a77cbb5398e106" { // sha1("one")
		t.Errorf("SHA1 = %q", o1.SHA1)
	}
	// Same bytes again: still a new version with a new FileID; zero modTime.
	if err := f.Upload(ctx, "a", strings.NewReader("one"), 3); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	o2 := listObject(t, f, "a")
	if o2.FileID == o1.FileID || o2.FileID == "" {
		t.Errorf("FileID %q after re-upload, want new (was %q)", o2.FileID, o1.FileID)
	}
	if !o2.SrcModified.IsZero() {
		t.Errorf("SrcModified after plain Upload = %v, want zero", o2.SrcModified)
	}
	if err := f.Upload(ctx, "b", strings.NewReader("two"), 3); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.Uploads, ","); got != "a,a,b" {
		t.Errorf("Uploads = %q, want a,a,b", got)
	}
}

// TestFakeUploadFailureNotLogged verifies a FailUpload failure stores nothing
// and is not logged in Uploads.
func TestFakeUploadFailureNotLogged(t *testing.T) {
	f := NewFake()
	f.FailUpload = func(string) error { return errors.New("boom") }
	if err := f.UploadWithModTime(context.Background(), "k", strings.NewReader("x"), 1, time.Now()); err == nil {
		t.Fatal("want error")
	}
	if len(f.Uploads) != 0 || len(f.Objects) != 0 || len(f.Meta) != 0 {
		t.Errorf("failed upload recorded: Uploads=%v Objects=%v Meta=%v", f.Uploads, f.Objects, f.Meta)
	}
}

// TestFakeDirectAssignmentFileID verifies objects assigned directly to Objects
// get a deterministic FileID that changes with the content, no times, and that
// direct reassignment after an Upload stops reporting that upload's Meta.
func TestFakeDirectAssignmentFileID(t *testing.T) {
	f := NewFake()
	f.Objects["k"] = []byte("v1")
	id1 := listObject(t, f, "k").FileID
	if id1 == "" || id1 != listObject(t, f, "k").FileID {
		t.Fatalf("FileID %q not deterministic", id1)
	}
	f.Objects["k"] = []byte("v2")
	o := listObject(t, f, "k")
	if o.FileID == id1 {
		t.Errorf("FileID unchanged after reassigning different bytes")
	}
	if !o.Modified.IsZero() || !o.SrcModified.IsZero() || o.SHA1 == "" {
		t.Errorf("direct object = %+v", o)
	}
	f.Objects["other"] = []byte("v2")
	if listObject(t, f, "other").FileID == o.FileID {
		t.Errorf("same content under another key shares FileID")
	}

	// Upload, then overwrite the bytes directly: the upload's Meta is stale.
	if err := f.UploadWithModTime(context.Background(), "k", strings.NewReader("v3"), 2, time.UnixMilli(5)); err != nil {
		t.Fatal(err)
	}
	up := listObject(t, f, "k")
	f.Objects["k"] = []byte("v4")
	o = listObject(t, f, "k")
	if o.FileID == up.FileID || !o.SrcModified.IsZero() {
		t.Errorf("stale Meta reported after direct reassignment: %+v", o)
	}
}

// TestFakeHandWrittenMeta verifies a Meta entry set directly by a test is
// honoured, with a derived FileID when it leaves FileID empty.
func TestFakeHandWrittenMeta(t *testing.T) {
	f := NewFake()
	f.Objects["k"] = []byte("data")
	mt := time.UnixMilli(77)
	f.Meta = map[string]FakeMeta{"k": {SrcModified: mt}}
	o := listObject(t, f, "k")
	if !o.SrcModified.Equal(mt) || o.FileID == "" {
		t.Errorf("hand-written Meta: %+v", o)
	}
	f.Meta["k"] = FakeMeta{FileID: "mine"}
	if got := listObject(t, f, "k").FileID; got != "mine" {
		t.Errorf("FileID = %q, want mine", got)
	}
}

// TestFakeDownloadLogAndDeleteMeta verifies Downloads logs successful
// downloads only, and Delete drops the key's Meta.
func TestFakeDownloadLogAndDeleteMeta(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	if err := f.UploadWithModTime(ctx, "k", strings.NewReader("x"), 1, time.UnixMilli(9)); err != nil {
		t.Fatal(err)
	}
	rc, _, err := f.Download(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if _, _, err := f.Download(ctx, "missing"); err == nil {
		t.Fatal("want error for missing key")
	}
	if got := strings.Join(f.Downloads, ","); got != "k" {
		t.Errorf("Downloads = %q, want k", got)
	}
	if err := f.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Meta["k"]; ok {
		t.Error("Delete kept Meta")
	}
	// Recreating the key directly must not resurrect the old upload's times.
	f.Objects["k"] = []byte("x")
	if o := listObject(t, f, "k"); !o.SrcModified.IsZero() || !o.Modified.IsZero() {
		t.Errorf("recreated object = %+v", o)
	}
}
