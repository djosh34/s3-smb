// Copyright 2026 s3-smb contributors. SPDX-License-Identifier: AGPL-3.0-only
// Narrow embedding additions to the pinned JuiceFS S3 implementation.
package object

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/djosh34/s3-smb/internal/logging"
)

// S3Options uses an already-resolved startup credential/TLS snapshot. Unlike the
// native CLI constructor, it never consults environment/profile credentials or
// guesses an HTTP endpoint from a hostname.
type S3Options struct {
	Bucket, Region, Endpoint, AccessKey, SecretKey, SessionToken string
	PathStyle                                                    *bool
	TLSConfig                                                    *tls.Config
}

func NewS3(o S3Options) (ObjectStorage, error) {
	if o.Bucket == "" || o.AccessKey == "" || o.SecretKey == "" {
		return nil, fmt.Errorf("S3 bucket and resolved credentials are required")
	}
	if o.Region == "" {
		o.Region = awsDefaultRegion
	}
	if o.Endpoint != "" {
		u, err := url.Parse(o.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("S3 endpoint must be an explicit HTTP(S) origin")
		}
	}
	// The cloned default transport already limits a dial to 30 seconds. A
	// request has no total limit, so a large metadata backup can finish.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	if o.TLSConfig != nil {
		if o.TLSConfig.InsecureSkipVerify {
			return nil, fmt.Errorf("TLS certificate verification cannot be disabled")
		}
		transport.TLSClientConfig = o.TLSConfig.Clone()
	}
	cfg := aws.Config{Region: o.Region, Credentials: credentials.NewStaticCredentialsProvider(o.AccessKey, o.SecretKey, o.SessionToken), HTTPClient: &http.Client{Transport: transport}, Logger: logging.SDKLogger{}}
	client := s3.NewFromConfig(cfg, func(opts *s3.Options) {
		opts.RetryMaxAttempts = 1 // native chunk layer owns data retries
		if o.Endpoint != "" {
			opts.BaseEndpoint = aws.String(o.Endpoint)
			opts.UsePathStyle = true
		}
		if o.PathStyle != nil {
			opts.UsePathStyle = *o.PathStyle
		}
	})
	return &s3client{bucket: o.Bucket, region: o.Region, s3: client}, nil
}

// PutIfAbsent is a conditional S3 publication, not a racy Head+Put. Callers must
// read back after errors: a lost response may have committed the exact object.
func (s *s3client) PutIfAbsent(ctx context.Context, key string, in io.Reader) error {
	_, err := s.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: in, IfNoneMatch: aws.String("*")})
	return err
}

// Close releases pooled HTTP connections; active operations must first stop.
func (s *s3client) Close() error {
	if c, ok := s.s3.Options().HTTPClient.(*http.Client); ok {
		c.CloseIdleConnections()
	}
	return nil
}
