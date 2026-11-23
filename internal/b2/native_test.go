package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"backuprepo/internal/apperr"
)

// b2TestServer simulates the subset of the B2 v3 API the client uses.
func b2TestServer(t *testing.T, store map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var base string
	// srcMillis holds each stored file's src_last_modified_millis file info, as
	// sent by b2_upload_file (X-Bz-Info-* header) or b2_start_large_file (fileInfo).
	srcMillis := map[string]string{}
	largeInfo := map[string]string{} // large fileId → pending src_last_modified_millis
	fileInfo := func(name string) map[string]string {
		if v, ok := srcMillis[name]; ok {
			return map[string]string{"src_last_modified_millis": v}
		}
		return nil
	}

	mux.HandleFunc("/b2api/v3/b2_authorize_account", func(w http.ResponseWriter, r *http.Request) {
		keyID, _, ok := r.BasicAuth()
		if !ok {
			w.WriteHeader(401)
			return
		}
		// keyID "restricted" simulates an application key restricted to
		// "my-bucket"; its token is checked by b2_list_buckets below.
		token := "test-token"
		if keyID == "restricted" {
			token = "restricted-token"
		}
		// v3 nests the storage-API endpoints under apiInfo.storageApi.
		json.NewEncoder(w).Encode(map[string]any{
			"authorizationToken": token,
			"accountId":          "acct-1",
			"apiInfo": map[string]any{
				"storageApi": map[string]any{
					"apiUrl":              base,
					"downloadUrl":         base,
					"recommendedPartSize": 100000000,
				},
			},
		})
	})
	mux.HandleFunc("/b2api/v3/b2_list_buckets", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AccountID  string `json:"accountId"`
			BucketName string `json:"bucketName"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.AccountID == "" {
			w.WriteHeader(400)
			return
		}
		// Like real B2: a bucket-restricted token must name its own bucket,
		// otherwise the call is unauthorized.
		if r.Header.Get("Authorization") == "restricted-token" && req.BucketName != "my-bucket" {
			w.WriteHeader(401)
			return
		}
		all := []map[string]any{
			{"bucketName": "my-bucket", "bucketId": "bid", "bucketType": "allPrivate"},
			{"bucketName": "other", "bucketId": "bid2", "bucketType": "allPublic"},
		}
		buckets := []map[string]any{}
		for _, bk := range all {
			if req.BucketName == "" || bk["bucketName"] == req.BucketName {
				buckets = append(buckets, bk)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"buckets": buckets})
	})
	mux.HandleFunc("/b2api/v3/b2_get_upload_url", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"uploadUrl":          base + "/upload",
			"authorizationToken": "upload-token",
		})
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		name := r.Header.Get("X-Bz-File-Name")
		body, _ := io.ReadAll(r.Body)
		store[name] = body
		if v := r.Header.Get("X-Bz-Info-src_last_modified_millis"); v != "" {
			srcMillis[name] = v
		} else {
			delete(srcMillis, name) // a new version without the info
		}
		json.NewEncoder(w).Encode(map[string]any{"fileName": name, "fileId": "id-" + name})
	})
	mux.HandleFunc("/b2api/v3/b2_list_file_names", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prefix        string `json:"prefix"`
			Delimiter     string `json:"delimiter"`
			StartFileName string `json:"startFileName"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		type f struct {
			FileName        string            `json:"fileName"`
			ContentLength   int64             `json:"contentLength"`
			ContentSha1     string            `json:"contentSha1,omitempty"`
			FileID          string            `json:"fileId,omitempty"`
			FileInfo        map[string]string `json:"fileInfo,omitempty"`
			UploadTimestamp int64             `json:"uploadTimestamp"`
			Action          string            `json:"action"`
		}
		var files []f
		if req.Delimiter == "/" {
			// Emulate B2 folder-grouping: emit synthetic folder entries for keys
			// whose remainder (after the prefix) contains a "/", deduplicated.
			seen := map[string]bool{}
			for name, data := range store {
				if !strings.HasPrefix(name, req.Prefix) {
					continue
				}
				rest := strings.TrimPrefix(name, req.Prefix)
				if idx := strings.Index(rest, "/"); idx >= 0 {
					// This key lives under a sub-folder; emit a folder entry.
					folder := req.Prefix + rest[:idx+1]
					if !seen[folder] {
						seen[folder] = true
						files = append(files, f{FileName: folder, ContentLength: 0, Action: "folder"})
					}
				} else {
					// Top-level file under prefix; emit as normal.
					files = append(files, f{FileName: name, ContentLength: int64(len(data)),
						FileInfo: fileInfo(name), Action: "upload"})
				}
			}
		} else {
			for name, data := range store {
				if strings.HasPrefix(name, req.Prefix) {
					sum := sha1.Sum(data)
					files = append(files, f{FileName: name, ContentLength: int64(len(data)),
						ContentSha1: hex.EncodeToString(sum[:]), FileID: "id-" + name,
						FileInfo: fileInfo(name), Action: "upload"})
				}
			}
		}
		// Real B2 returns file names in ascending order; map iteration doesn't.
		sort.Slice(files, func(i, j int) bool { return files[i].FileName < files[j].FileName })
		json.NewEncoder(w).Encode(map[string]any{"files": files, "nextFileName": nil})
	})
	mux.HandleFunc("/b2api/v3/b2_list_file_versions", func(w http.ResponseWriter, r *http.Request) {
		// Mirror real B2: reject a startFileId sent without (or with an empty)
		// startFileName, which is what the first-page Delete request used to do.
		var raw map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &raw)
		if sid, ok := raw["startFileId"]; ok {
			if name, _ := raw["startFileName"].(string); name == "" || sid == "" {
				w.WriteHeader(400)
				return
			}
		}
		var req struct{ Prefix string }
		json.Unmarshal(body, &req)
		type f struct {
			FileName string `json:"fileName"`
			FileID   string `json:"fileId"`
		}
		var files []f
		for name := range store {
			if name == req.Prefix {
				files = append(files, f{FileName: name, FileID: "id-" + name})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"files": files, "nextFileName": nil, "nextFileId": nil})
	})
	mux.HandleFunc("/b2api/v3/b2_delete_file_version", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ FileName, FileId string }
		json.NewDecoder(r.Body).Decode(&req)
		delete(store, req.FileName)
		delete(srcMillis, req.FileName)
		json.NewEncoder(w).Encode(map[string]any{"fileName": req.FileName, "fileId": req.FileId})
	})
	mux.HandleFunc("/file/", func(w http.ResponseWriter, r *http.Request) {
		// path: /file/{bucketName}/{key...}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/file/"), "/", 2)
		if len(parts) != 2 {
			w.WriteHeader(404)
			return
		}
		data, ok := store[parts[1]]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(data)
	})
	mux.HandleFunc("/b2api/v3/b2_start_large_file", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FileName string            `json:"fileName"`
			FileInfo map[string]string `json:"fileInfo"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		largeInfo["large-"+req.FileName] = req.FileInfo["src_last_modified_millis"]
		json.NewEncoder(w).Encode(map[string]any{"fileId": "large-" + req.FileName, "fileName": req.FileName})
	})
	mux.HandleFunc("/b2api/v3/b2_get_upload_part_url", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FileId string `json:"fileId"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]any{"uploadUrl": base + "/uploadpart?fileId=" + req.FileId, "authorizationToken": "part-token"})
	})
	largeParts := map[string][][]byte{}
	mux.HandleFunc("/uploadpart", func(w http.ResponseWriter, r *http.Request) {
		fileID := r.URL.Query().Get("fileId")
		body, _ := io.ReadAll(r.Body)
		largeParts[fileID] = append(largeParts[fileID], body)
		json.NewEncoder(w).Encode(map[string]any{"partNumber": len(largeParts[fileID])})
	})
	mux.HandleFunc("/b2api/v3/b2_finish_large_file", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			FileID string `json:"fileId"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		name := strings.TrimPrefix(req.FileID, "large-")
		var full []byte
		for _, p := range largeParts[req.FileID] {
			full = append(full, p...)
		}
		store[name] = full
		if v := largeInfo[req.FileID]; v != "" {
			srcMillis[name] = v
		} else {
			delete(srcMillis, name)
		}
		delete(largeInfo, req.FileID)
		json.NewEncoder(w).Encode(map[string]any{"fileName": name, "fileId": req.FileID})
	})

	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

// testB2 builds a B2Backend pointed at the given test server.
func testB2(t *testing.T, srv *httptest.Server) *B2Backend {
	t.Helper()
	b := newB2Backend(Config{
		BucketName: "my-bucket", BucketID: "bid", KeyID: "k", AppKey: "a",
	})
	b.authURL = srv.URL
	b.http = srv.Client()
	return b
}

// TestB2UploadDownload verifies a small file round-trips through Upload and Download.
func TestB2UploadDownload(t *testing.T) {
	store := map[string][]byte{}
	srv := b2TestServer(t, store)
	b := testB2(t, srv)
	ctx := context.Background()

	if err := b.Upload(ctx, "dir/file.txt", bytes.NewReader([]byte("hello b2")), 8); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if string(store["dir/file.txt"]) != "hello b2" {
		t.Fatalf("server stored %q", store["dir/file.txt"])
	}
	rc, n, err := b.Download(ctx, "dir/file.txt")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello b2" || n != 8 {
		t.Fatalf("download mismatch %q n=%d", got, n)
	}
}

// TestB2DownloadMissing verifies downloading an absent key returns ErrObjectNotFound.
func TestB2DownloadMissing(t *testing.T) {
	srv := b2TestServer(t, map[string][]byte{})
	b := testB2(t, srv)
	if _, _, err := b.Download(context.Background(), "nope"); !errors.Is(err, apperr.ErrObjectNotFound) {
		t.Fatalf("want ErrObjectNotFound, got %v", err)
	}
}

// TestB2ListAndDelete verifies recursive List returns nested objects and Delete removes a key.
func TestB2ListAndDelete(t *testing.T) {
	store := map[string][]byte{"a.txt": []byte("1"), "p/b.txt": []byte("2"), "p/c.txt": []byte("3")}
	srv := b2TestServer(t, store)
	b := testB2(t, srv)
	ctx := context.Background()

	l, err := b.List(ctx, "p/", true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Objects) != 2 {
		t.Fatalf("recursive list under p/ = %d objects, want 2", len(l.Objects))
	}
	if err := b.Delete(ctx, "p/b.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := store["p/b.txt"]; ok {
		t.Fatal("p/b.txt should be deleted")
	}
}

// TestB2LargeFileUpload verifies the multipart large-file path reassembles parts correctly.
func TestB2LargeFileUpload(t *testing.T) {
	store := map[string][]byte{}
	srv := b2TestServer(t, store)
	b := testB2(t, srv)
	b.partSize = 5 // tiny parts so a small payload exercises the multipart path
	ctx := context.Background()

	payload := []byte("abcdefghijklmnop") // 16 bytes → 4 parts of 5,5,5,1
	err := b.uploadLarge(ctx, mustAuth(t, b, ctx), "big.bin", bytes.NewReader(payload), int64(len(payload)), nil)
	if err != nil {
		t.Fatalf("uploadLarge: %v", err)
	}
	if string(store["big.bin"]) != string(payload) {
		t.Fatalf("reassembled = %q want %q", store["big.bin"], payload)
	}
}

// mustAuth authorizes b and fails the test if authorization errors.
func mustAuth(t *testing.T, b *B2Backend, ctx context.Context) *b2Auth {
	t.Helper()
	a, err := b.authorize(ctx)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	return a
}

// TestB2ListNonRecursiveGroupsFolders verifies non-recursive List groups subfolders into Prefixes.
func TestB2ListNonRecursiveGroupsFolders(t *testing.T) {
	store := map[string][]byte{
		"a.txt":            []byte("1"),
		"photos/1.jpg":     []byte("2"),
		"photos/2.jpg":     []byte("3"),
		"photos/sub/3.jpg": []byte("4"),
	}
	srv := b2TestServer(t, store)
	b := testB2(t, srv)
	ctx := context.Background()

	l, err := b.List(ctx, "", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(l.Objects) != 1 || l.Objects[0].Key != "a.txt" {
		t.Fatalf("objects = %+v, want [a.txt]", l.Objects)
	}
	if len(l.Prefixes) != 1 || l.Prefixes[0] != "photos/" {
		t.Fatalf("prefixes = %+v, want [photos/]", l.Prefixes)
	}
}

// flakyUploadServer simulates B2 auth + get_upload_url + an upload endpoint that
// returns `failStatus` for its first `failN` POSTs, then succeeds. It reports how
// many upload attempts it saw. Used to exercise the upload retry loop.
func flakyUploadServer(t *testing.T, store map[string][]byte, failN int, failStatus int, hits *int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/b2api/v3/b2_authorize_account", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"authorizationToken": "t", "accountId": "acct",
			"apiInfo": map[string]any{"storageApi": map[string]any{
				"apiUrl": base, "downloadUrl": base, "recommendedPartSize": 100000000}},
		})
	})
	mux.HandleFunc("/b2api/v3/b2_get_upload_url", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"uploadUrl": base + "/upload", "authorizationToken": "u"})
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(hits, 1)
		if int(n) <= failN {
			w.WriteHeader(failStatus)
			return
		}
		name := r.Header.Get("X-Bz-File-Name")
		body, _ := io.ReadAll(r.Body)
		store[name] = body
		json.NewEncoder(w).Encode(map[string]any{"fileName": name, "fileId": "id-" + name})
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

// TestB2UploadRetriesTransient verifies a small-file upload retries past transient
// 503s (a new upload URL each attempt) and ultimately succeeds.
func TestB2UploadRetriesTransient(t *testing.T) {
	store := map[string][]byte{}
	var hits int32
	srv := flakyUploadServer(t, store, 2, http.StatusServiceUnavailable, &hits)
	b := testB2(t, srv)

	if err := b.Upload(context.Background(), "f.txt", bytes.NewReader([]byte("hi")), 2); err != nil {
		t.Fatalf("Upload should retry past 503s: %v", err)
	}
	if string(store["f.txt"]) != "hi" {
		t.Fatalf("server stored %q, want \"hi\"", store["f.txt"])
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("upload attempts = %d, want 3 (2 failed + 1 success)", got)
	}
}

// TestB2UploadFailsFastOnNonRetryable verifies a non-retryable status (400) is
// NOT retried — one attempt, immediate error.
func TestB2UploadFailsFastOnNonRetryable(t *testing.T) {
	store := map[string][]byte{}
	var hits int32
	srv := flakyUploadServer(t, store, 99, http.StatusBadRequest, &hits) // always 400
	b := testB2(t, srv)

	err := b.Upload(context.Background(), "f.txt", bytes.NewReader([]byte("hi")), 2)
	if !errors.Is(err, apperr.ErrUploadFailed) {
		t.Fatalf("want ErrUploadFailed, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upload attempts = %d, want 1 (400 is not retryable)", got)
	}
}

// TestB2UploadGivesUpAfterMaxAttempts verifies persistent transient failures stop
// after b2MaxUploadAttempts and return an error.
func TestB2UploadGivesUpAfterMaxAttempts(t *testing.T) {
	store := map[string][]byte{}
	var hits int32
	srv := flakyUploadServer(t, store, 99, http.StatusServiceUnavailable, &hits) // always 503
	b := testB2(t, srv)

	err := b.Upload(context.Background(), "f.txt", bytes.NewReader([]byte("hi")), 2)
	if !errors.Is(err, apperr.ErrUploadFailed) {
		t.Fatalf("want ErrUploadFailed after exhausting retries, got %v", err)
	}
	if got := atomic.LoadInt32(&hits); int(got) != b2MaxUploadAttempts {
		t.Fatalf("upload attempts = %d, want %d", got, b2MaxUploadAttempts)
	}
}

// TestB2ListBuckets verifies ListBuckets returns each bucket's name and ID.
func TestB2ListBuckets(t *testing.T) {
	srv := b2TestServer(t, map[string][]byte{})
	b := testB2(t, srv)

	list, err := b.ListBuckets(context.Background())
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d buckets, want 2", len(list))
	}
	if list[0].Name != "my-bucket" || list[0].ID != "bid" || list[0].Type != "allPrivate" {
		t.Fatalf("bucket[0] = %+v", list[0])
	}
	if list[1].Name != "other" || list[1].ID != "bid2" {
		t.Fatalf("bucket[1] = %+v", list[1])
	}
}

// testB2Key is testB2 with a specific keyID ("restricted" makes the test server
// treat the key as restricted to "my-bucket").
func testB2Key(t *testing.T, srv *httptest.Server, keyID string) *B2Backend {
	t.Helper()
	b := testB2(t, srv)
	b.cfg.KeyID = keyID
	return b
}

// TestB2FindBucketRestrictedKey is the regression for bucket-ID auto-resolve
// failing with a bucket-scoped key: B2 denies an unfiltered b2_list_buckets for
// such keys, so the lookup must name the bucket.
func TestB2FindBucketRestrictedKey(t *testing.T) {
	srv := b2TestServer(t, map[string][]byte{})
	ctx := context.Background()

	// The old lookup path (list everything) is denied for a restricted key.
	if _, err := testB2Key(t, srv, "restricted").ListBuckets(ctx); !errors.Is(err, apperr.ErrListFailed) {
		t.Fatalf("ListBuckets with restricted key: err = %v, want ErrListFailed", err)
	}
	// Naming the bucket succeeds and returns just that bucket.
	list, err := testB2Key(t, srv, "restricted").FindBucket(ctx, "my-bucket")
	if err != nil {
		t.Fatalf("FindBucket: %v", err)
	}
	if len(list) != 1 || list[0].Name != "my-bucket" || list[0].ID != "bid" {
		t.Fatalf("FindBucket = %+v, want [my-bucket bid]", list)
	}
	// A bucket outside the key's restriction is refused, not silently empty.
	if _, err := testB2Key(t, srv, "restricted").FindBucket(ctx, "other"); !errors.Is(err, apperr.ErrListFailed) {
		t.Fatalf("FindBucket(other) with restricted key: err = %v, want ErrListFailed", err)
	}
}

// TestB2FindBucketAccountKey verifies FindBucket filters by name for an
// account-wide key and returns an empty list (no error) for an unknown name.
func TestB2FindBucketAccountKey(t *testing.T) {
	srv := b2TestServer(t, map[string][]byte{})
	ctx := context.Background()

	list, err := testB2(t, srv).FindBucket(ctx, "other")
	if err != nil {
		t.Fatalf("FindBucket: %v", err)
	}
	if len(list) != 1 || list[0].ID != "bid2" {
		t.Fatalf("FindBucket(other) = %+v, want [other bid2]", list)
	}
	list, err = testB2(t, srv).FindBucket(ctx, "missing")
	if err != nil || len(list) != 0 {
		t.Fatalf("FindBucket(missing) = %+v, %v; want empty, nil", list, err)
	}
}

// TestB2FindBucketEmptyName verifies an empty name is rejected instead of
// silently falling back to listing every bucket.
func TestB2FindBucketEmptyName(t *testing.T) {
	srv := b2TestServer(t, map[string][]byte{})
	if _, err := testB2(t, srv).FindBucket(context.Background(), ""); !errors.Is(err, apperr.ErrListFailed) {
		t.Fatalf("FindBucket(\"\"): err = %v, want ErrListFailed", err)
	}
}

// TestB2AuthRejectsEmptyCreds verifies operations fail with ErrInvalidCredentials when creds are empty.
func TestB2AuthRejectsEmptyCreds(t *testing.T) {
	store := map[string][]byte{}
	srv := b2TestServer(t, store)
	b := newB2Backend(Config{}) // empty KeyID and AppKey
	b.authURL = srv.URL
	b.http = srv.Client()
	ctx := context.Background()

	// List is a representative operation that triggers authorize().
	_, err := b.List(ctx, "", false)
	if !errors.Is(err, apperr.ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
}

var _ Backend = (*B2Backend)(nil)

// TestListBucketsAccessDenied checks the typed status lets callers tell a
// refused request (a scoped key listing every bucket) from other failures via
// apperr.ErrAccessDenied, while the message keeps its old "status N" text.
func TestListBucketsAccessDenied(t *testing.T) {
	srv := b2TestServer(t, map[string][]byte{})
	_, err := testB2Key(t, srv, "restricted").ListBuckets(context.Background())
	if !errors.Is(err, apperr.ErrAccessDenied) || !errors.Is(err, apperr.ErrListFailed) {
		t.Fatalf("restricted ListBuckets: err = %v, want ErrAccessDenied + ErrListFailed", err)
	}
	if !strings.Contains(err.Error(), "status 401") {
		t.Errorf("error text changed: %v", err)
	}
	for _, e := range []error{errors.New("status 401"), statusError(500), fmt.Errorf("x: %w", statusError(404))} {
		if errors.Is(e, apperr.ErrAccessDenied) {
			t.Errorf("errors.Is(%v, ErrAccessDenied) = true", e)
		}
	}
	if !errors.Is(fmt.Errorf("x: %w", statusError(403)), apperr.ErrAccessDenied) {
		t.Error("wrapped 403 should match ErrAccessDenied")
	}
}

// TestB2RemoteSHA1 checks RemoteSHA1 reports the contentSha1 B2 lists for an
// existing key (also carried in List results) and found=false for a missing
// one, without matching a longer name that merely starts with the key.
func TestB2RemoteSHA1(t *testing.T) {
	data := map[string][]byte{"docs/a.txt": []byte("hello"), "docs/a.txt.bak": []byte("other")}
	srv := b2TestServer(t, data)
	be := testB2(t, srv)
	ctx := context.Background()
	got, found, err := be.RemoteSHA1(ctx, "docs/a.txt")
	if err != nil || !found {
		t.Fatalf("RemoteSHA1(existing) = %q, %v, %v", got, found, err)
	}
	if want := "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d"; got != want {
		t.Errorf("sha1 = %q, want %q", got, want)
	}
	if _, found, err := be.RemoteSHA1(ctx, "docs/missing.txt"); err != nil || found {
		t.Errorf("RemoteSHA1(missing) found=%v err=%v, want not found", found, err)
	}
	l, err := be.List(ctx, "docs/", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range l.Objects {
		if o.SHA1 == "" || o.FileID != "id-"+o.Key {
			t.Errorf("List object %s: SHA1 %q, FileID %q", o.Key, o.SHA1, o.FileID)
		}
	}
}

// TestSrcModified parses B2's src_last_modified_millis file info, treating a
// missing, malformed or non-positive value as unknown (zero time).
func TestSrcModified(t *testing.T) {
	if got := srcModified(map[string]string{"src_last_modified_millis": "1700000000123"}); !got.Equal(time.UnixMilli(1700000000123)) {
		t.Errorf("srcModified = %v", got)
	}
	for _, info := range []map[string]string{nil, {}, {"src_last_modified_millis": "x"}, {"src_last_modified_millis": "0"}} {
		if got := srcModified(info); !got.IsZero() {
			t.Errorf("srcModified(%v) = %v, want zero", info, got)
		}
	}
}
