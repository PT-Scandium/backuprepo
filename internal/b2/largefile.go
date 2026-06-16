package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
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
			sha, err := b.uploadPart(ctx, auth, start.FileID, partNum, buf[:n])
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
func (b *B2Backend) uploadPart(ctx context.Context, auth *b2Auth, fileID string, num int, data []byte) (string, error) {
	var up struct {
		UploadURL          string `json:"uploadUrl"`
		AuthorizationToken string `json:"authorizationToken"`
	}
	if err := b.postJSON(ctx, auth, "b2_get_upload_part_url",
		map[string]string{"fileId": fileID}, &up); err != nil {
		return "", fmt.Errorf("%w: get_upload_part_url: %v", apperr.ErrUploadFailed, err)
	}
	sum := sha1.Sum(data)
	sha := hex.EncodeToString(sum[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, up.UploadURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("%w: %v", apperr.ErrUploadFailed, err)
	}
	req.Header.Set("Authorization", up.AuthorizationToken)
	req.Header.Set("X-Bz-Part-Number", strconv.Itoa(num))
	req.Header.Set("X-Bz-Content-Sha1", sha)
	req.Header.Set("Content-Length", strconv.Itoa(len(data)))
	resp, err := b.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: upload part %d: %v", apperr.ErrUploadFailed, num, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: upload part %d: status %d", apperr.ErrUploadFailed, num, resp.StatusCode)
	}
	return sha, nil
}
