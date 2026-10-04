// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type fakeListing struct {
	calls int
	next  func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
}

func (f *fakeListing) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.calls++
	return f.next(input)
}

func TestCompletePaginatedInventory(t *testing.T) {
	now := time.Now().UTC()
	fake := &fakeListing{next: func(input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
		if aws.ToString(input.Bucket) != "owned" || aws.ToString(input.Prefix) != "v/meta/" {
			t.Fatal("wrong inventory scope")
		}
		if input.ContinuationToken == nil {
			return &s3.ListObjectsV2Output{IsTruncated: aws.Bool(true), NextContinuationToken: aws.String("next"), Contents: []types.Object{{Key: aws.String("v/meta/snapshot-1"), Size: aws.Int64(12), LastModified: &now, ETag: aws.String("opaque")}}}, nil
		}
		if aws.ToString(input.ContinuationToken) != "next" {
			t.Fatal("wrong continuation")
		}
		return &s3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String("v/meta/snapshot-2"), Size: aws.Int64(23), LastModified: &now}}}, nil
	}}
	result, err := listBucket(context.Background(), fake, "owned", "v/meta/")
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 2 || result.ObjectCount != 2 || result.TotalBytes != 35 || result.ListedAt.Before(now) || result.Objects[0].ETag != "opaque" {
		t.Fatalf("bad inventory: %+v", result)
	}
}

func TestEmptyInventoryAndListingFailure(t *testing.T) {
	fake := &fakeListing{next: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) { return &s3.ListObjectsV2Output{}, nil }}
	result, err := listBucket(context.Background(), fake, "owned", "")
	if err != nil || result.ObjectCount != 0 || result.Objects == nil {
		t.Fatal("empty inventory not explicit")
	}
	data, _ := json.Marshal(result)
	if !bytes.Contains(data, []byte(`"objects":[]`)) {
		t.Fatal("empty result must be []")
	}
	fake.next = func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
		return nil, errors.New("secret server error")
	}
	if _, err = listBucket(context.Background(), fake, "owned", ""); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("listing error lost or leaked")
	}
}

func TestTruncatedInventoryCannotClaimComplete(t *testing.T) {
	for _, token := range []string{"", "repeated"} {
		fake := &fakeListing{next: func(*s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			return &s3.ListObjectsV2Output{IsTruncated: aws.Bool(true), NextContinuationToken: aws.String(token)}, nil
		}}
		if _, err := listBucket(context.Background(), fake, "owned", ""); err == nil {
			t.Fatal("accepted incomplete inventory")
		}
		if fake.calls > 2 {
			t.Fatal("unbounded pagination")
		}
	}
}

func TestBucketCLIUsesExistingSDKAndEnvironment(t *testing.T) {
	t.Setenv("MINIO_ROOT_USER", "test-only-access")
	t.Setenv("MINIO_ROOT_PASSWORD", "test-only-secret")
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=test-only-access/") {
			t.Error("SDK did not sign using explicit fixture credentials")
		}
		requests <- r.Method + " " + r.URL.Path
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>owned</Name><IsTruncated>false</IsTruncated><KeyCount>0</KeyCount></ListBucketResult>`))
		}
	}))
	defer server.Close()
	for _, command := range []string{"bucket-create", "bucket-list"} {
		var out bytes.Buffer
		if err := run([]string{command, "--endpoint", server.URL, "--bucket", "owned"}, &out); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out.Bytes(), []byte("test-only")) {
			t.Fatal("credential in CLI output")
		}
		if command == "bucket-list" && !bytes.Contains(out.Bytes(), []byte(`"object_count":0`)) {
			t.Fatal("missing explicit empty proof")
		}
	}
	if <-requests != "PUT /owned" || <-requests != "GET /owned" {
		t.Fatal("unexpected SDK requests")
	}
}
