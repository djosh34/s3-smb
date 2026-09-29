// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type inventoryObject struct {
	Key          string     `json:"key"`
	Size         int64      `json:"size"`
	LastModified *time.Time `json:"last_modified"`
	ETag         string     `json:"etag"`
}

type inventory struct {
	Bucket      string            `json:"bucket"`
	Prefix      string            `json:"prefix"`
	ListedAt    time.Time         `json:"listed_at"`
	ObjectCount int               `json:"object_count"`
	TotalBytes  int64             `json:"total_bytes"`
	Objects     []inventoryObject `json:"objects"`
}

type bucketLister interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

func listBucket(ctx context.Context, client bucketLister, bucket, prefix string) (inventory, error) {
	result := inventory{Bucket: bucket, Prefix: prefix, Objects: []inventoryObject{}}
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)}, func(o *s3.ListObjectsV2PaginatorOptions) {
		o.StopOnDuplicateToken = true
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return inventory{}, errors.New("bucket listing failed")
		}
		for _, object := range page.Contents {
			result.Objects = append(result.Objects, inventoryObject{Key: aws.ToString(object.Key), Size: aws.ToInt64(object.Size), LastModified: object.LastModified, ETag: aws.ToString(object.ETag)})
			result.TotalBytes += aws.ToInt64(object.Size)
		}
		// Never silently accept an incomplete/repeating listing as empty proof.
		if aws.ToBool(page.IsTruncated) && (!paginator.HasMorePages() || aws.ToString(page.NextContinuationToken) == "") {
			return inventory{}, errors.New("bucket listing truncated without progress")
		}
	}
	result.ListedAt = time.Now().UTC()
	result.ObjectCount = len(result.Objects)
	return result, nil
}

func bucketCommand(command string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	endpoint := flags.String("endpoint", "http://127.0.0.1:19000", "task-private MinIO origin")
	bucket := flags.String("bucket", "", "task-owned bucket")
	prefix := flags.String("prefix", "", "list only keys beginning with prefix")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *bucket == "" || (command == "bucket-create" && *prefix != "") {
		return errors.New("bucket command requires --bucket NAME and no positional arguments")
	}
	if _, err := loopbackURL(*endpoint); err != nil {
		return err
	}
	access, secret := os.Getenv("MINIO_ROOT_USER"), os.Getenv("MINIO_ROOT_PASSWORD")
	if access == "" || secret == "" {
		return errors.New("MINIO_ROOT_USER and MINIO_ROOT_PASSWORD are required")
	}
	client := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: endpoint, UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""),
		HTTPClient: &http.Client{
			Transport:     &http.Transport{Proxy: nil},
			Timeout:       30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if command == "bucket-create" {
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
			return errors.New("bucket creation failed")
		}
		return json.NewEncoder(out).Encode(map[string]any{"bucket": *bucket, "created_at": time.Now().UTC()})
	}
	result, err := listBucket(ctx, client, *bucket, *prefix)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
