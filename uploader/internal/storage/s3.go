package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/jonkeane/publish-to-r2/uploader/internal/config"
	"github.com/jonkeane/publish-to-r2/uploader/internal/credentials"
	"github.com/jonkeane/publish-to-r2/uploader/internal/manifest"
)

type S3 struct {
	client *s3.Client
	bucket string
}

func New(p config.Profile, secret credentials.Secret) *S3 {
	c := s3.New(s3.Options{
		Region: "auto", BaseEndpoint: aws.String(p.Endpoint), UsePathStyle: true,
		Credentials:                awscreds.NewStaticCredentialsProvider(secret.AccessKeyID, secret.SecretAccessKey, ""),
		HTTPClient:                 &http.Client{Timeout: 90 * time.Second},
		Retryer:                    retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 4; o.MaxBackoff = 5 * time.Second }),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &S3{client: c, bucket: p.Bucket}
}
func safeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return ErrNotFound
		case "PreconditionFailed", "ConditionalRequestConflict":
			return ErrConflict
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "ExpiredToken":
			return ErrPermission
		}
	}
	return ErrNetwork // Never echo SDK errors, signed headers, or credential material.
}
func (s *S3) Head(ctx context.Context, key string) (Object, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	x, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return Object{}, safeError(err)
	}
	return Object{Key: key, Size: aws.ToInt64(x.ContentLength), Hash: x.Metadata["sha256"], ETag: aws.ToString(x.ETag), Modified: aws.ToTime(x.LastModified)}, nil
}
func (s *S3) Get(ctx context.Context, key string) ([]byte, Object, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	x, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, Object{}, safeError(err)
	}
	defer x.Body.Close()
	b, err := io.ReadAll(io.LimitReader(x.Body, manifest.MaxJSON+1))
	if err != nil {
		return nil, Object{}, safeError(err)
	}
	if len(b) > manifest.MaxJSON {
		return nil, Object{}, errors.New("remote JSON exceeds size limit")
	}
	return b, Object{Key: key, Size: aws.ToInt64(x.ContentLength), Hash: x.Metadata["sha256"], ETag: aws.ToString(x.ETag), Modified: aws.ToTime(x.LastModified)}, nil
}
func (s *S3) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, o PutOptions) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: body, ContentLength: &size, ContentType: &o.ContentType, CacheControl: &o.CacheControl}
	if o.Hash != "" {
		in.Metadata = map[string]string{"sha256": o.Hash}
	}
	if o.Match != "" {
		in.IfMatch = &o.Match
	}
	if o.Create {
		in.IfNoneMatch = aws.String("*")
	}
	_, err := s.client.PutObject(ctx, in)
	return safeError(err)
}
func (s *S3) Copy(ctx context.Context, source, destination string, o CopyOptions) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	copySource := url.PathEscape(s.bucket + "/" + source)
	in := &s3.CopyObjectInput{
		Bucket:            &s.bucket,
		Key:               &destination,
		CopySource:        &copySource,
		MetadataDirective: types.MetadataDirectiveReplace,
		ContentType:       &o.ContentType,
		CacheControl:      &o.CacheControl,
	}
	if o.Hash != "" {
		in.Metadata = map[string]string{"sha256": o.Hash}
	}
	_, err := s.client.CopyObject(ctx, in)
	return safeError(err)
}
func (s *S3) Delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return safeError(err)
}
func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	out := []Object{}
	for pager.HasMorePages() {
		pageCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		page, err := pager.NextPage(pageCtx)
		cancel()
		if err != nil {
			return nil, safeError(err)
		}
		for _, x := range page.Contents {
			out = append(out, Object{Key: aws.ToString(x.Key), Size: aws.ToInt64(x.Size), ETag: aws.ToString(x.ETag), Modified: aws.ToTime(x.LastModified)})
		}
	}
	return out, nil
}
