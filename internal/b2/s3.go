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

const multipartPartSize = 100 * 1024 * 1024

// S3Backend talks to a B2 bucket via the S3-compatible API.
type S3Backend struct {
	client *s3.Client
	bucket string
}

// newS3Backend builds an S3-compatible backend for the configured B2 bucket and endpoint.
func newS3Backend(ctx context.Context, cfg Config) (*S3Backend, error) {
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
		o.UsePathStyle = true
	})
	return &S3Backend{client: client, bucket: cfg.BucketName}, nil
}

// Upload stores key, using S3 multipart upload for large files via the manager.
func (u *S3Backend) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
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

// Exists reports whether key is present via a HeadObject call.
func (u *S3Backend) Exists(ctx context.Context, key string) (bool, error) {
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

// Download streams key, returning apperr.ErrObjectNotFound if it does not exist.
func (u *S3Backend) Download(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := u.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(u.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		var nf *types.NotFound
		if errors.As(err, &nsk) || errors.As(err, &nf) {
			return nil, 0, fmt.Errorf("%w: %s", apperr.ErrObjectNotFound, key)
		}
		return nil, 0, fmt.Errorf("%w: get %s: %v", apperr.ErrDownloadFailed, key, err)
	}
	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

// List returns objects under prefix; when not recursive, a "/" delimiter groups
// immediate subfolders into Prefixes.
func (u *S3Backend) List(ctx context.Context, prefix string, recursive bool) (Listing, error) {
	var out Listing
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(u.bucket),
		Prefix: aws.String(prefix),
	}
	if !recursive {
		in.Delimiter = aws.String("/")
	}
	p := s3.NewListObjectsV2Paginator(u.client, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return Listing{}, fmt.Errorf("%w: list %s: %v", apperr.ErrListFailed, prefix, err)
		}
		for _, obj := range page.Contents {
			info := ObjectInfo{Key: aws.ToString(obj.Key)}
			if obj.Size != nil {
				info.Size = *obj.Size
			}
			if obj.LastModified != nil {
				info.Modified = *obj.LastModified
			}
			out.Objects = append(out.Objects, info)
		}
		for _, cp := range page.CommonPrefixes {
			out.Prefixes = append(out.Prefixes, aws.ToString(cp.Prefix))
		}
	}
	return out, nil
}

// Delete removes key, deleting all versions if the bucket is versioned and
// falling back to a plain delete otherwise.
func (u *S3Backend) Delete(ctx context.Context, key string) error {
	// Delete every version if the bucket is versioned; otherwise the single object.
	vp := s3.NewListObjectVersionsPaginator(u.client, &s3.ListObjectVersionsInput{
		Bucket: aws.String(u.bucket),
		Prefix: aws.String(key),
	})
	var ids []types.ObjectIdentifier
	for vp.HasMorePages() {
		page, err := vp.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("%w: list versions %s: %v", apperr.ErrDeleteFailed, key, err)
		}
		for _, v := range page.Versions {
			if aws.ToString(v.Key) == key {
				ids = append(ids, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
			}
		}
		for _, dm := range page.DeleteMarkers {
			if aws.ToString(dm.Key) == key {
				ids = append(ids, types.ObjectIdentifier{Key: dm.Key, VersionId: dm.VersionId})
			}
		}
	}
	if len(ids) == 0 {
		// Fall back to a plain delete (covers non-versioned buckets that report no versions).
		_, err := u.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(u.bucket), Key: aws.String(key),
		})
		if err != nil {
			return fmt.Errorf("%w: delete %s: %v", apperr.ErrDeleteFailed, key, err)
		}
		return nil
	}
	_, err := u.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(u.bucket),
		Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return fmt.Errorf("%w: delete versions %s: %v", apperr.ErrDeleteFailed, key, err)
	}
	return nil
}
