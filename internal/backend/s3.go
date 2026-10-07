package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3 works with AWS, Backblaze B2, Cloudflare R2, Wasabi, Garage, SeaweedFS,
// Ceph RGW and any other S3-compatible service.
type S3 struct {
	client *s3.Client
	cfg    Config
}

func NewS3(ctx context.Context, c Config, creds Credentials) (*S3, error) {
	if c.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if creds.AccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(creds.AccessKey, creds.SecretKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
			o.UsePathStyle = true
		}
	})
	return &S3{client: client, cfg: c}, nil
}

func (s *S3) key(k string) string {
	if s.cfg.Prefix == "" {
		return k
	}
	return path.Join(s.cfg.Prefix, k)
}

func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	in := &s3.PutObjectInput{
		Bucket:        aws.String(s.cfg.Bucket),
		Key:           aws.String(s.key(key)),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
	}
	if s.cfg.ObjectLockMode != "" && s.cfg.ObjectLockDays > 0 {
		in.ObjectLockMode = types.ObjectLockMode(s.cfg.ObjectLockMode)
		in.ObjectLockRetainUntilDate = aws.Time(time.Now().Add(time.Duration(s.cfg.ObjectLockDays) * 24 * time.Hour))
	}
	_, err := s.client.PutObject(ctx, in)
	return err
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(s.key(key))})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func (s *S3) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(s.key(key))})
	if err != nil {
		var nf *types.NotFound
		if errors.As(err, &nf) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: aws.ToInt64(out.ContentLength), Modified: aws.ToTime(out.LastModified)}, nil
}

func (s *S3) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.cfg.Bucket),
		Prefix: aws.String(s.key(prefix)),
	})
	strip := ""
	if s.cfg.Prefix != "" {
		strip = strings.TrimSuffix(s.cfg.Prefix, "/") + "/"
	}
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			if err := fn(ObjectInfo{
				Key:      strings.TrimPrefix(aws.ToString(o.Key), strip),
				Size:     aws.ToInt64(o.Size),
				Modified: aws.ToTime(o.LastModified),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(s.key(key))})
	return err
}

func (s *S3) Location() string {
	loc := fmt.Sprintf("s3://%s/%s", s.cfg.Bucket, s.cfg.Prefix)
	if s.cfg.Endpoint != "" {
		loc += " @ " + s.cfg.Endpoint
	}
	return loc
}

func (s *S3) Close() error { return nil }

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(s.key(key))})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return out.Body, nil
}
