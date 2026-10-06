//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

// b2Backup makes a first backup, one incremental and one restore against the
// B2 test bucket over the real network. The tree stays small, because B2
// costs money. The workflow empties the bucket afterwards.
func (h *harness) b2Backup() result {
	outcome := h.baseline()
	outcome.Scenario = "b2"
	latest, tree := h.incremental("incremental", outcome.Baseline, func() {
		h.randomFile("later.bin", 64<<20)
		h.must(h.proofDir.WriteFile("nested/message.txt", []byte("changed after the baseline\n"), 0o600))
	})
	outcome.Resumed = latest
	outcome.ResumedRestore = h.restoreBackup(latest, tree, "restore-latest")
	return outcome
}

// useB2 points s3-smb at the B2 test bucket that the workflow passes in. The
// keys stay in the environment.
func (h *harness) useB2() {
	for _, name := range []string{"B2_KEY_ID", "B2_APPLICATION_KEY", "B2_ENDPOINT", "B2_BUCKET"} {
		if os.Getenv(name) == "" {
			h.t.Fatal(name + " is required for the b2 scenario")
		}
	}
	region, err := helpers.B2Region(os.Getenv("B2_ENDPOINT"))
	h.must(err)
	h.b2, h.endpoint, h.bucket, h.region = true, os.Getenv("B2_ENDPOINT"), os.Getenv("B2_BUCKET"), region
}

func b2Client(region string) *s3.Client {
	return s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: aws.String(os.Getenv("B2_ENDPOINT")),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(os.Getenv("B2_KEY_ID"), os.Getenv("B2_APPLICATION_KEY"), ""),
	})
}

// connectB2 connects to the B2 test bucket, which must be empty.
func (h *harness) connectB2() {
	h.s3 = b2Client(h.region)
	if len(h.objects("")) != 0 {
		h.t.Fatal("the B2 test bucket is not empty")
	}
}

// TestEmptyB2Bucket deletes every object version in the B2 test bucket by ID
// and fails if any is left. The b2 job runs it after the backup, also when
// that failed.
func TestEmptyB2Bucket(t *testing.T) {
	region, err := helpers.B2Region(os.Getenv("B2_ENDPOINT"))
	if err != nil {
		t.Fatal(err)
	}
	client, bucket := b2Client(region), os.Getenv("B2_BUCKET")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	for range 3 {
		var left int
		pages := s3.NewListObjectVersionsPaginator(client, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var ids [][2]*string
			for _, v := range page.Versions {
				ids = append(ids, [2]*string{v.Key, v.VersionId})
			}
			for _, m := range page.DeleteMarkers {
				ids = append(ids, [2]*string{m.Key, m.VersionId})
			}
			left += len(ids)
			for _, id := range ids {
				if _, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: id[0], VersionId: id[1]}); err != nil {
					t.Error(err)
				}
			}
		}
		t.Log("deleted object versions", left)
		if left == 0 {
			return
		}
	}
	t.Error("object versions are left in the B2 test bucket")
}
