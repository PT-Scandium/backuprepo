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

// b2APIVersion is the native B2 API version segment used in endpoint paths.
const b2APIVersion = "v3"

// B2Backend talks to Backblaze via the native B2 v3 API.
type B2Backend struct {
	cfg      Config
	http     *http.Client
	authURL  string
	auth     *b2Auth
	partSize int64
}

type b2Auth struct {
	AccountID           string
	APIURL              string
	DownloadURL         string
	Token               string
	RecommendedPartSize int64
}

// newB2Backend builds a native B2 backend with the default auth URL and part size.
func newB2Backend(cfg Config) *B2Backend {
	return &B2Backend{cfg: cfg, http: http.DefaultClient, authURL: defaultB2AuthURL, partSize: b2SmallFileLimit}
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
		b.authURL+"/b2api/"+b2APIVersion+"/b2_authorize_account", nil)
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
	// In v3, the storage-API endpoints (apiUrl, downloadUrl, recommendedPartSize)
	// are nested under apiInfo.storageApi; authorizationToken and accountId are
	// top-level. accountId is required by account-scoped calls like b2_list_buckets.
	var out struct {
		AuthorizationToken string `json:"authorizationToken"`
		AccountID          string `json:"accountId"`
		APIInfo            struct {
			StorageAPI struct {
				APIURL              string `json:"apiUrl"`
				DownloadURL         string `json:"downloadUrl"`
				RecommendedPartSize int64  `json:"recommendedPartSize"`
			} `json:"storageApi"`
		} `json:"apiInfo"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", apperr.ErrAuthFailed, err)
	}
	b.auth = &b2Auth{
		AccountID:           out.AccountID,
		APIURL:              out.APIInfo.StorageAPI.APIURL,
		DownloadURL:         out.APIInfo.StorageAPI.DownloadURL,
		Token:               out.AuthorizationToken,
		RecommendedPartSize: out.APIInfo.StorageAPI.RecommendedPartSize,
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
		auth.APIURL+"/b2api/"+b2APIVersion+"/"+endpoint, bytes.NewReader(buf))
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

// b2MaxUploadAttempts bounds how many times an upload (or upload-part) POST is
// retried, each with a freshly fetched upload URL, before giving up. B2 hands out
// a per-pod upload URL, and an individual pod can be briefly unreachable (TCP
// connection refused/reset/timeout) or return 5xx; the documented remedy is to
// request a NEW upload URL — usually a different, healthy pod — and retry.
const b2MaxUploadAttempts = 5

// Upload stores key. Small files go through b2_upload_file; larger files use the
// large-file API (see largefile.go). Transient upload failures are retried with a
// fresh upload URL (see b2MaxUploadAttempts).
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
	sha := hex.EncodeToString(sha1Sum(data))

	getURL := func() (string, string, error) {
		a, err := b.authorize(ctx)
		if err != nil {
			return "", "", err
		}
		var up struct {
			UploadURL          string `json:"uploadUrl"`
			AuthorizationToken string `json:"authorizationToken"`
		}
		if err := b.postJSON(ctx, a, "b2_get_upload_url",
			map[string]string{"bucketId": b.cfg.BucketID}, &up); err != nil {
			return "", "", err
		}
		return up.UploadURL, up.AuthorizationToken, nil
	}
	setHeaders := func(req *http.Request) {
		req.Header.Set("X-Bz-File-Name", encodeFileName(key))
		req.Header.Set("Content-Type", "b2/x-auto")
		req.Header.Set("X-Bz-Content-Sha1", sha)
	}
	return b.uploadWithRetry(ctx, "upload "+key, getURL, setHeaders, data)
}

// uploadWithRetry POSTs data to a B2 upload URL, retrying up to
// b2MaxUploadAttempts times on transient failures. getURL is called before every
// attempt to fetch a fresh upload URL + token (so a retry lands on a new pod);
// setHeaders stamps the endpoint-specific headers (Authorization and
// Content-Length are set here). desc labels the operation in errors.
func (b *B2Backend) uploadWithRetry(ctx context.Context, desc string,
	getURL func() (url, token string, err error), setHeaders func(*http.Request), data []byte) error {

	var lastErr error
	for attempt := 1; attempt <= b2MaxUploadAttempts; attempt++ {
		if attempt > 1 {
			if err := b2Backoff(ctx, attempt); err != nil {
				return err
			}
		}
		url, token, err := getURL()
		if err != nil {
			lastErr = err // fetching a URL can itself fail transiently; retry
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("%w: %s: %v", apperr.ErrUploadFailed, desc, err)
		}
		req.Header.Set("Authorization", token)
		req.Header.Set("Content-Length", strconv.Itoa(len(data)))
		setHeaders(req)

		resp, err := b.http.Do(req)
		if err != nil {
			lastErr = err // connection refused/reset/timeout → retry with a new URL
			continue
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code == http.StatusOK {
			return nil
		}
		lastErr = fmt.Errorf("status %d", code)
		if code == http.StatusUnauthorized {
			b.auth = nil // upload token expired → force re-authorize on the next getURL
		}
		if !b2RetryableStatus(code) {
			return fmt.Errorf("%w: %s: status %d", apperr.ErrUploadFailed, desc, code)
		}
	}
	return fmt.Errorf("%w: %s: giving up after %d attempts: %v",
		apperr.ErrUploadFailed, desc, b2MaxUploadAttempts, lastErr)
}

// b2RetryableStatus reports whether an HTTP status from a B2 upload warrants a
// retry with a fresh upload URL (transient/server-side or an expired token).
func b2RetryableStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, // 401 — expired upload/auth token
		http.StatusRequestTimeout,      // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// b2Backoff waits before retry `attempt` (2..N): 200ms, 400ms, 800ms, … capped
// at 3s, aborting early if ctx is cancelled.
func b2Backoff(ctx context.Context, attempt int) error {
	d := 200 * time.Millisecond * time.Duration(1<<(attempt-2))
	if d > 3*time.Second {
		d = 3 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// sha1Sum returns the SHA-1 of data as a byte slice (small helper so callers can
// hex-encode without juggling the array type).
func sha1Sum(data []byte) []byte {
	sum := sha1.Sum(data)
	return sum[:]
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

// ListBuckets returns every bucket the credentials can see, with the bucket ID
// the native list/upload calls need. Only the native B2 API exposes bucket IDs,
// so this has no S3Backend equivalent.
func (b *B2Backend) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	auth, err := b.authorize(ctx)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buckets []struct {
			BucketName string `json:"bucketName"`
			BucketID   string `json:"bucketId"`
			BucketType string `json:"bucketType"`
		} `json:"buckets"`
	}
	if err := b.postJSON(ctx, auth, "b2_list_buckets",
		map[string]string{"accountId": auth.AccountID}, &resp); err != nil {
		return nil, fmt.Errorf("%w: list buckets: %v", apperr.ErrListFailed, err)
	}
	out := make([]BucketInfo, 0, len(resp.Buckets))
	for _, bk := range resp.Buckets {
		out = append(out, BucketInfo{Name: bk.BucketName, ID: bk.BucketID, Type: bk.BucketType})
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
			"bucketId":     b.cfg.BucketID,
			"prefix":       key,
			"maxFileCount": 1000,
		}
		// B2 rejects startFileId unless a non-empty startFileName accompanies
		// it, and an empty startFileName cursor is unnecessary on the first page.
		if start != "" {
			body["startFileName"] = start
			if startID != "" {
				body["startFileId"] = startID
			}
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

// msToTime converts a B2 millisecond timestamp to time.Time; 0 yields the zero time.
func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
