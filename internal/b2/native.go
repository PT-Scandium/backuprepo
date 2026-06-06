package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"backuprepo/internal/apperr"
)

// b2SmallFileLimit is the threshold above which uploads must use the large-file
// (multipart) API. Files at or below this size use a single b2_upload_file call.
const b2SmallFileLimit = 100 * 1024 * 1024

const defaultB2AuthURL = "https://api.backblazeb2.com"

// B2Backend talks to Backblaze via the native B2 v2 API.
type B2Backend struct {
	cfg     Config
	http    *http.Client
	authURL string
	auth    *b2Auth
}

type b2Auth struct {
	APIURL              string
	DownloadURL         string
	Token               string
	RecommendedPartSize int64
}

func newB2Backend(cfg Config) *B2Backend {
	return &B2Backend{cfg: cfg, http: http.DefaultClient, authURL: defaultB2AuthURL}
}

// authorize fetches and caches an auth context.
func (b *B2Backend) authorize(ctx context.Context) (*b2Auth, error) {
	if b.auth != nil {
		return b.auth, nil
	}
	if b.cfg.KeyID == "" || b.cfg.AppKey == "" {
		return nil, apperr.ErrInvalidCredentials
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.authURL+"/b2api/v2/b2_authorize_account", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", apperr.ErrAuthFailed, err)
	}
	req.SetBasicAuth(b.cfg.KeyID, b.cfg.AppKey)
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", apperr.ErrAuthFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", apperr.ErrAuthFailed, resp.StatusCode)
	}
	var out struct {
		APIURL              string `json:"apiUrl"`
		DownloadURL         string `json:"downloadUrl"`
		AuthorizationToken  string `json:"authorizationToken"`
		RecommendedPartSize int64  `json:"recommendedPartSize"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", apperr.ErrAuthFailed, err)
	}
	b.auth = &b2Auth{
		APIURL: out.APIURL, DownloadURL: out.DownloadURL,
		Token: out.AuthorizationToken, RecommendedPartSize: out.RecommendedPartSize,
	}
	return b.auth, nil
}

// postJSON calls a B2 API endpoint with a JSON body and the auth token.
func (b *B2Backend) postJSON(ctx context.Context, auth *b2Auth, endpoint string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		auth.APIURL+"/b2api/v2/"+endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Upload stores key. Small files go through b2_upload_file; larger files use the
// large-file API (see largefile.go).
func (b *B2Backend) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	auth, err := b.authorize(ctx)
	if err != nil {
		return err
	}
	if size > b2SmallFileLimit {
		return b.uploadLarge(ctx, auth, key, r, size)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", apperr.ErrUploadFailed, key, err)
	}

	var up struct {
		UploadURL          string `json:"uploadUrl"`
		AuthorizationToken string `json:"authorizationToken"`
	}
	if err := b.postJSON(ctx, auth, "b2_get_upload_url",
		map[string]string{"bucketId": b.cfg.BucketID}, &up); err != nil {
		return fmt.Errorf("%w: get_upload_url: %v", apperr.ErrUploadFailed, err)
	}

	sum := sha1.Sum(data)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.UploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("%w: %v", apperr.ErrUploadFailed, err)
	}
	req.Header.Set("Authorization", up.AuthorizationToken)
	req.Header.Set("X-Bz-File-Name", encodeFileName(key))
	req.Header.Set("Content-Type", "b2/x-auto")
	req.Header.Set("X-Bz-Content-Sha1", hex.EncodeToString(sum[:]))
	req.Header.Set("Content-Length", strconv.Itoa(len(data)))
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: upload %s: %v", apperr.ErrUploadFailed, key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: upload %s: status %d", apperr.ErrUploadFailed, key, resp.StatusCode)
	}
	return nil
}

// Download streams key by name from the download URL.
func (b *B2Backend) Download(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	auth, err := b.authorize(ctx)
	if err != nil {
		return nil, 0, err
	}
	url := auth.DownloadURL + "/file/" + b.cfg.BucketName + "/" + encodeFileName(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", apperr.ErrDownloadFailed, err)
	}
	req.Header.Set("Authorization", auth.Token)
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", apperr.ErrDownloadFailed, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("%w: %s", apperr.ErrObjectNotFound, key)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("%w: status %d", apperr.ErrDownloadFailed, resp.StatusCode)
	}
	return resp.Body, resp.ContentLength, nil
}

// Exists reports whether key is present (via a single-name list).
func (b *B2Backend) Exists(ctx context.Context, key string) (bool, error) {
	l, err := b.listRaw(ctx, key, "", 1)
	if err != nil {
		return false, err
	}
	for _, o := range l {
		if o.Key == key {
			return true, nil
		}
	}
	return false, nil
}

type b2File struct {
	FileName        string `json:"fileName"`
	ContentLength   int64  `json:"contentLength"`
	UploadTimestamp int64  `json:"uploadTimestamp"`
	Action          string `json:"action"`
}

// listRaw returns objects under prefix (recursive, no delimiter), handling pagination.
func (b *B2Backend) listRaw(ctx context.Context, prefix, delimiter string, max int) ([]ObjectInfo, error) {
	auth, err := b.authorize(ctx)
	if err != nil {
		return nil, err
	}
	var out []ObjectInfo
	start := ""
	for {
		var resp struct {
			Files        []b2File `json:"files"`
			NextFileName *string  `json:"nextFileName"`
		}
		body := map[string]any{
			"bucketId":      b.cfg.BucketID,
			"prefix":        prefix,
			"startFileName": start,
			"maxFileCount":  1000,
		}
		if delimiter != "" {
			body["delimiter"] = delimiter
		}
		if err := b.postJSON(ctx, auth, "b2_list_file_names", body, &resp); err != nil {
			return nil, fmt.Errorf("%w: list %s: %v", apperr.ErrListFailed, prefix, err)
		}
		for _, f := range resp.Files {
			out = append(out, ObjectInfo{
				Key:      f.FileName,
				Size:     f.ContentLength,
				Modified: msToTime(f.UploadTimestamp),
			})
			if max > 0 && len(out) >= max {
				return out, nil
			}
		}
		if resp.NextFileName == nil || *resp.NextFileName == "" {
			return out, nil
		}
		start = *resp.NextFileName
	}
}

// List groups results folder-like when not recursive.
func (b *B2Backend) List(ctx context.Context, prefix string, recursive bool) (Listing, error) {
	delim := "/"
	if recursive {
		delim = ""
	}
	raw, err := b.listRaw(ctx, prefix, delim, 0)
	if err != nil {
		return Listing{}, err
	}
	var out Listing
	for _, o := range raw {
		// With a delimiter, B2 returns folder entries as keys ending in "/".
		if !recursive && strings.HasSuffix(o.Key, "/") {
			out.Prefixes = append(out.Prefixes, o.Key)
			continue
		}
		out.Objects = append(out.Objects, o)
	}
	return out, nil
}

// Delete removes all versions of key.
func (b *B2Backend) Delete(ctx context.Context, key string) error {
	auth, err := b.authorize(ctx)
	if err != nil {
		return err
	}
	type ver struct {
		FileName string `json:"fileName"`
		FileID   string `json:"fileId"`
	}
	var found int
	start, startID := "", ""
	for {
		var resp struct {
			Files        []ver   `json:"files"`
			NextFileName *string `json:"nextFileName"`
			NextFileID   *string `json:"nextFileId"`
		}
		body := map[string]any{
			"bucketId":      b.cfg.BucketID,
			"prefix":        key,
			"startFileName": start,
			"startFileId":   startID,
			"maxFileCount":  1000,
		}
		if err := b.postJSON(ctx, auth, "b2_list_file_versions", body, &resp); err != nil {
			return fmt.Errorf("%w: list versions %s: %v", apperr.ErrDeleteFailed, key, err)
		}
		for _, v := range resp.Files {
			if v.FileName != key {
				continue
			}
			found++
			if err := b.postJSON(ctx, auth, "b2_delete_file_version",
				map[string]string{"fileName": v.FileName, "fileId": v.FileID}, nil); err != nil {
				return fmt.Errorf("%w: delete %s: %v", apperr.ErrDeleteFailed, key, err)
			}
		}
		if resp.NextFileName == nil || *resp.NextFileName == "" {
			break
		}
		start = *resp.NextFileName
		if resp.NextFileID != nil {
			startID = *resp.NextFileID
		}
	}
	if found == 0 {
		return fmt.Errorf("%w: %s", apperr.ErrObjectNotFound, key)
	}
	return nil
}

// encodeFileName percent-encodes a B2 file name while preserving path slashes.
func encodeFileName(name string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = urlEncodeSegment(s)
	}
	return strings.Join(segs, "/")
}

// urlEncodeSegment encodes a single path segment per RFC 3986 (no slash).
func urlEncodeSegment(s string) string {
	const upper = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upper[c>>4])
		b.WriteByte(upper[c&0xF])
	}
	return b.String()
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
