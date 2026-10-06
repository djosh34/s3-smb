// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// object is one entry of a LIST. Modified is the backend's Last-Modified.
type object struct {
	modified time.Time
	key      string
}

// objects is everything the engine asks of S3: no conditional writes, no
// multipart and no versions. Each call retries on its own and gives up with an
// error once its time budget is spent.
type objects interface {
	put(ctx context.Context, key string, data []byte) error
	// get reads length bytes from offset, or the whole object when length is
	// zero. A missing key returns an error matching errNotFound.
	get(ctx context.Context, key string, offset, length uint64) ([]byte, error)
	remove(ctx context.Context, key string) error
	list(ctx context.Context, prefix string) ([]object, error)
}

var errNotFound = errors.New("object not found")

// BucketOptions selects an S3 bucket. TLS is optional; set it to use custom
// trust roots or a client certificate. PathStyle addresses the bucket in the
// URL path, as MinIO needs.
type BucketOptions struct {
	TLS          *tls.Config
	Endpoint     string
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	PathStyle    bool
}

// Bucket reaches one S3 bucket with PUT, GET, DELETE and LIST only.
type Bucket struct {
	client  *s3.Client
	name    string
	prefix  string // keeps test runs apart in a shared bucket
	timeout time.Duration
}

// requestTimeout bounds one call, retries included. It covers a 5-minute
// outage with a minute to spare.
const requestTimeout = 6 * time.Minute

// NewBucket builds the client. The SDK's own retries and its default upload
// checksum stay on. The retry quota is off, so an outage cannot use it up, and
// each call retries until requestTimeout.
func NewBucket(options BucketOptions) (*Bucket, error) {
	return newBucket(options, retry.DefaultMaxBackoff)
}

func newBucket(options BucketOptions, maxBackoff time.Duration) (*Bucket, error) {
	if options.Bucket == "" || options.Region == "" {
		return nil, errors.New("bucket and region are required")
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: time.Minute, TLSClientConfig: options.TLS,
		IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 16, TLSHandshakeTimeout: 30 * time.Second,
	}}
	config := aws.Config{
		Region:      options.Region,
		HTTPClient:  client,
		Credentials: credentials.NewStaticCredentialsProvider(options.AccessKey, options.SecretKey, options.SessionToken),
		Retryer: func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = 1 << 20
				o.MaxBackoff = maxBackoff
				o.RateLimiter = ratelimit.None
			})
		},
	}
	s3client := s3.NewFromConfig(config, func(o *s3.Options) {
		o.UsePathStyle = options.PathStyle
		if options.Endpoint != "" {
			o.BaseEndpoint = aws.String(options.Endpoint)
		}
	})
	return &Bucket{client: s3client, name: options.Bucket, timeout: requestTimeout}, nil
}

func (b *Bucket) put(ctx context.Context, key string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.name), Key: aws.String(b.prefix + key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))),
	})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// get also retries a body that breaks off, which the SDK leaves to us.
func (b *Bucket) get(ctx context.Context, key string, offset, length uint64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	input := &s3.GetObjectInput{Bucket: aws.String(b.name), Key: aws.String(b.prefix + key)}
	if length > 0 {
		input.Range = aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	}
	for {
		data, err := b.getOnce(ctx, input, length)
		if err == nil || !errors.Is(err, errBrokenBody) {
			if err != nil {
				return nil, fmt.Errorf("get %s: %w", key, err)
			}
			return data, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("get %s: %w", key, errors.Join(err, ctx.Err()))
		case <-time.After(time.Second):
		}
	}
}

var errBrokenBody = errors.New("response body broke off")

func (b *Bucket) getOnce(ctx context.Context, input *s3.GetObjectInput, length uint64) ([]byte, error) {
	out, err := b.client.GetObject(ctx, input)
	if err != nil {
		var missing *types.NoSuchKey
		var api smithy.APIError
		if errors.As(err, &missing) || errors.As(err, &api) && api.ErrorCode() == "NotFound" {
			return nil, errors.Join(errNotFound, err)
		}
		return nil, err
	}
	data, err := io.ReadAll(out.Body)
	if closeErr := out.Body.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err == nil && out.ContentLength != nil && int64(len(data)) != *out.ContentLength {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, errors.Join(errBrokenBody, err)
	}
	if length > 0 && uint64(len(data)) > length {
		return nil, fmt.Errorf("got %d bytes for a %d byte range", len(data), length)
	}
	return data, nil
}

func (b *Bucket) remove(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	if _, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.name), Key: aws.String(b.prefix + key)}); err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

func (b *Bucket) list(ctx context.Context, prefix string) ([]object, error) {
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	var result []object
	pages := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{Bucket: aws.String(b.name), Prefix: aws.String(b.prefix + prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, item := range page.Contents {
			key, ok := strings.CutPrefix(aws.ToString(item.Key), b.prefix)
			if !ok || !strings.HasPrefix(key, prefix) || item.LastModified == nil {
				return nil, fmt.Errorf("list %s: malformed entry %q", prefix, key)
			}
			result = append(result, object{key: key, modified: *item.LastModified})
		}
	}
	return result, nil
}
