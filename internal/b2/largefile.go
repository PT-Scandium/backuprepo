package b2

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"backuprepo/internal/apperr"
)

// uploadLarge uploads key in parts via the B2 large-file API.
func (b *B2Backend) uploadLarge(ctx context.Context, auth *b2Auth, key string, r io.Reader, size int64) error {
	part := b.partSize
	if part <= 0 {
		part = b2SmallFileLimit
	}

	// 1. Start the large file.
	var start struct {
		FileID string `json:"fileId"`
	}
	if err := b.postJSON(ctx, auth, "b2_start_large_file", map[string]string{
		"bucketId": b.cfg.BucketID, "fileName": encodeFileName(key), "contentType": "b2/x-auto",
	}, &start); err != nil {
		return fmt.Errorf("%w: start_large_file %s: %v", apperr.ErrUploadFailed, key, err)
	}

	// 2. Upload each part, collecting SHA-1s.
	var shas []string
	buf := make([]byte, part)
	partNum := 0
	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			partNum++
			sha, err := b.uploadPart(ctx, start.FileID, partNum, buf[:n])
			if err != nil {
				return err
			}
			shas = append(shas, sha)
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("%w: read %s: %v", apperr.ErrUploadFailed, key, readErr)
		}
	}

	// 3. Finish.
	if err := b.postJSON(ctx, auth, "b2_finish_large_file", map[string]any{
		"fileId": start.FileID, "partSha1Array": shas,
	}, nil); err != nil {
		return fmt.Errorf("%w: finish_large_file %s: %v", apperr.ErrUploadFailed, key, err)
	}
	return nil
}

// uploadPart uploads one part of a large file and returns its SHA-1 hash.
// Transient failures are retried with a fresh part-upload URL (see
// uploadWithRetry / b2MaxUploadAttempts).
func (b *B2Backend) uploadPart(ctx context.Context, fileID string, num int, data []byte) (string, error) {
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
		if err := b.postJSON(ctx, a, "b2_get_upload_part_url",
			map[string]string{"fileId": fileID}, &up); err != nil {
			return "", "", err
		}
		return up.UploadURL, up.AuthorizationToken, nil
	}
	setHeaders := func(req *http.Request) {
		req.Header.Set("X-Bz-Part-Number", strconv.Itoa(num))
		req.Header.Set("X-Bz-Content-Sha1", sha)
	}
	if err := b.uploadWithRetry(ctx, fmt.Sprintf("upload part %d", num), getURL, setHeaders, data); err != nil {
		return "", err
	}
	return sha, nil
}
