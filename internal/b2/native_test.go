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
	"testing"

	"backuprepo/internal/apperr"
)

// b2TestServer simulates the subset of the B2 v2 API the client uses.
func b2TestServer(t *testing.T, store map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var base string

	mux.HandleFunc("/b2api/v2/b2_authorize_account", func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"apiUrl":              base,
			"downloadUrl":         base,
			"authorizationToken":  "test-token",
			"recommendedPartSize": 100000000,
		})
	})
	mux.HandleFunc("/b2api/v2/b2_get_upload_url", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"uploadUrl":          base + "/upload",
			"authorizationToken": "upload-token",
		})
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		name := r.Header.Get("X-Bz-File-Name")
		body, _ := io.ReadAll(r.Body)
		store[name] = body
		json.NewEncoder(w).Encode(map[string]any{"fileName": name, "fileId": "id-" + name})
	})
	mux.HandleFunc("/b2api/v2/b2_list_file_names", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prefix, Delimiter, StartFileName string
		}
		json.NewDecoder(r.Body).Decode(&req)
		type f struct {
			FileName        string `json:"fileName"`
			ContentLength   int64  `json:"contentLength"`
			UploadTimestamp int64  `json:"uploadTimestamp"`
			Action          string `json:"action"`
		}
		var files []f
		for name, data := range store {
			if strings.HasPrefix(name, req.Prefix) {
				files = append(files, f{FileName: name, ContentLength: int64(len(data)), Action: "upload"})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"files": files, "nextFileName": nil})
	})
	mux.HandleFunc("/b2api/v2/b2_list_file_versions", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Prefix string }
		json.NewDecoder(r.Body).Decode(&req)
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
	mux.HandleFunc("/b2api/v2/b2_delete_file_version", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ FileName, FileId string }
		json.NewDecoder(r.Body).Decode(&req)
		delete(store, req.FileName)
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

	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

func testB2(t *testing.T, srv *httptest.Server) *B2Backend {
	t.Helper()
	b := newB2Backend(Config{
		BucketName: "my-bucket", BucketID: "bid", KeyID: "k", AppKey: "a",
	})
	b.authURL = srv.URL
	b.http = srv.Client()
	return b
}

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

func TestB2DownloadMissing(t *testing.T) {
	srv := b2TestServer(t, map[string][]byte{})
	b := testB2(t, srv)
	if _, _, err := b.Download(context.Background(), "nope"); !errors.Is(err, apperr.ErrObjectNotFound) {
		t.Fatalf("want ErrObjectNotFound, got %v", err)
	}
}

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

func TestB2LargeFileNotYetImplemented(t *testing.T) {
	srv := b2TestServer(t, map[string][]byte{})
	b := testB2(t, srv)
	big := bytes.NewReader(make([]byte, 1)) // size arg drives the branch
	err := b.Upload(context.Background(), "big.bin", big, b2SmallFileLimit+1)
	if !errors.Is(err, apperr.ErrUploadFailed) {
		t.Fatalf("want ErrUploadFailed for oversize, got %v", err)
	}
}

var _ Backend = (*B2Backend)(nil)
