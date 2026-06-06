package b2

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"backuprepo/internal/apperr"
)

// multipartPartSize is the part size for the transfer manager. The manager
// automatically uses a single PutObject for small files and multipart for
// large ones, satisfying the spec's 100 MB threshold.
const multipartPartSize = 100 * 1024 * 1024

// S3Uploader uploads to a B2 bucket via the S3-compatible API.
type S3Uploader struct {
	client *s3.Client
	bucket string
}

// NewS3Uploader builds an S3 client pointed at the B2 endpoint using static creds.
func NewS3Uploader(ctx context.Context, cfg Config) (*S3Uploader, error) {
	if cfg.KeyID == "" || cfg.AppKey == "" {
		return nil, apperr.ErrInvalidCredentials
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.KeyID, cfg.AppKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: aws config: %v", apperr.ErrUploadFailed, err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = true // B2 S3 works reliably with path-style addressing
	})
	return &S3Uploader{client: client, bucket: cfg.Bucket}, nil
}

// Upload stores r under key, auto-selecting single vs multipart by size.
func (u *S3Uploader) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	uploader := manager.NewUploader(u.client, func(m *manager.Uploader) {
		m.PartSize = multipartPartSize
	})
	_, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("%w: put %s: %v", apperr.ErrUploadFailed, key, err)
	}
	return nil
}

// Exists reports whether key is present in the bucket.
func (u *S3Uploader) Exists(ctx context.Context, key string) (bool, error) {
	_, err := u.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	var notFound *types.NotFound
	var noKey *types.NoSuchKey
	if errors.As(err, &notFound) || errors.As(err, &noKey) {
		return false, nil
	}
	return false, fmt.Errorf("%w: head %s: %v", apperr.ErrUploadFailed, key, err)
}
