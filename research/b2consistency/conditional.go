package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

const wrongETag = `"00000000000000000000000000000000"`

func quote(etag string) string { return `"` + etag + `"` }

// code returns the HTTP status of a call, success or not, with the S3 error code.
func code(md middleware.Metadata, err error) string {
	if err != nil {
		return status(err)
	}
	if resp, ok := awsmiddleware.GetRawResponse(md).(*smithyhttp.Response); ok {
		return fmt.Sprint(resp.StatusCode)
	}
	return "2xx"
}

// testConditionalFull records, for each conditional request, the exact status
// and what happened to the object. For the record; nothing here is an anomaly.
// Each case runs 3 times on fresh keys.
func (r *runner) testConditionalFull(ctx context.Context) {
	res := map[string]map[string]int{}
	rec := func(name, st, outcome string) {
		if res[name] == nil {
			res[name] = map[string]int{}
		}
		res[name][st+"; "+outcome]++
	}
	// state tells what a key holds now compared with its old and attempted bodies.
	state := func(key string, before, attempted []byte) string {
		b, found, err := r.get(ctx, key)
		switch {
		case err != nil:
			return "read error"
		case !found:
			return "absent"
		case before != nil && bytes.Equal(b, before):
			return "unchanged"
		case bytes.Equal(b, attempted):
			return "new body"
		default:
			return "other body"
		}
	}
	for rep := range 3 {
		dir := fmt.Sprintf("%scond2/%d/", r.prefix, rep)
		n := 0
		// fresh returns a new key and, if exists, puts a body there first.
		fresh := func(exists bool) (string, []byte, string, bool) {
			n++
			key := fmt.Sprintf("%s%02d", dir, n)
			if !exists {
				return key, nil, "", true
			}
			body := randBody(300 + n)
			etag, err := r.put(ctx, key, body)
			return key, body, etag, err == nil
		}

		// PutObject.
		putCase := func(name string, exists, deleted bool, set func(in *s3.PutObjectInput, etag string)) {
			key, before, etag, ok := fresh(exists)
			if !ok {
				return
			}
			if deleted && r.del(ctx, key) != nil {
				return
			}
			if deleted {
				before = nil
			}
			body := randBody(200)
			in := &s3.PutObjectInput{Bucket: &r.bucket, Key: &key, Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))}
			set(in, etag)
			out, err := r.w.PutObject(ctx, in)
			var md middleware.Metadata
			if out != nil {
				md = out.ResultMetadata
			}
			rec(name, code(md, err), state(key, before, body))
		}
		putCase("put if-none-match * absent", false, false, func(in *s3.PutObjectInput, _ string) { in.IfNoneMatch = aws.String("*") })
		putCase("put if-none-match * exists", true, false, func(in *s3.PutObjectInput, _ string) { in.IfNoneMatch = aws.String("*") })
		putCase("put if-none-match * deleted", true, true, func(in *s3.PutObjectInput, _ string) { in.IfNoneMatch = aws.String("*") })
		putCase("put if-match right", true, false, func(in *s3.PutObjectInput, e string) { in.IfMatch = aws.String(quote(e)) })
		putCase("put if-match wrong", true, false, func(in *s3.PutObjectInput, _ string) { in.IfMatch = aws.String(wrongETag) })
		putCase("put if-match absent", false, false, func(in *s3.PutObjectInput, _ string) { in.IfMatch = aws.String(wrongETag) })

		// CompleteMultipartUpload. After a refused complete, abort tells
		// whether the upload was still open (204) or already gone.
		mpuCase := func(name string, exists bool, set func(in *s3.CompleteMultipartUploadInput, etag string)) {
			key, before, etag, ok := fresh(exists)
			if !ok {
				return
			}
			cr, err := r.w.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &r.bucket, Key: &key})
			if r.failed("mpu_create", err) {
				return
			}
			body := randBody(200)
			up, err := r.w.UploadPart(ctx, &s3.UploadPartInput{Bucket: &r.bucket, Key: &key, UploadId: cr.UploadId,
				PartNumber: aws.Int32(1), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))})
			if r.failed("mpu_part", err) {
				return
			}
			in := &s3.CompleteMultipartUploadInput{Bucket: &r.bucket, Key: &key, UploadId: cr.UploadId,
				MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: up.ETag, PartNumber: aws.Int32(1)}}}}
			set(in, etag)
			out, err := r.w.CompleteMultipartUpload(ctx, in)
			var md middleware.Metadata
			if out != nil {
				md = out.ResultMetadata
			}
			outcome := state(key, before, body)
			if err != nil {
				ab, aerr := r.w.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &r.bucket, Key: &key, UploadId: cr.UploadId})
				var amd middleware.Metadata
				if ab != nil {
					amd = ab.ResultMetadata
				}
				outcome += ", abort " + code(amd, aerr)
			}
			rec(name, code(md, err), outcome)
		}
		mpuCase("complete if-none-match * absent", false, func(in *s3.CompleteMultipartUploadInput, _ string) { in.IfNoneMatch = aws.String("*") })
		mpuCase("complete if-none-match * exists", true, func(in *s3.CompleteMultipartUploadInput, _ string) { in.IfNoneMatch = aws.String("*") })
		mpuCase("complete if-match right", true, func(in *s3.CompleteMultipartUploadInput, e string) { in.IfMatch = aws.String(quote(e)) })
		mpuCase("complete if-match wrong", true, func(in *s3.CompleteMultipartUploadInput, _ string) { in.IfMatch = aws.String(wrongETag) })

		// CopyObject, with destination and source conditions.
		src, srcBody, srcETag, ok := fresh(true)
		if !ok {
			continue
		}
		copyCase := func(name string, exists bool, set func(in *s3.CopyObjectInput, etag string)) {
			key, before, etag, ok := fresh(exists)
			if !ok {
				return
			}
			in := &s3.CopyObjectInput{Bucket: &r.bucket, Key: &key, CopySource: aws.String(r.bucket + "/" + src)}
			set(in, etag)
			out, err := r.w.CopyObject(ctx, in)
			var md middleware.Metadata
			if out != nil {
				md = out.ResultMetadata
			}
			rec(name, code(md, err), state(key, before, srcBody))
		}
		copyCase("copy dest if-none-match * absent", false, func(in *s3.CopyObjectInput, _ string) { in.IfNoneMatch = aws.String("*") })
		copyCase("copy dest if-none-match * exists", true, func(in *s3.CopyObjectInput, _ string) { in.IfNoneMatch = aws.String("*") })
		copyCase("copy dest if-match right", true, func(in *s3.CopyObjectInput, e string) { in.IfMatch = aws.String(quote(e)) })
		copyCase("copy dest if-match wrong", true, func(in *s3.CopyObjectInput, _ string) { in.IfMatch = aws.String(wrongETag) })
		copyCase("copy source if-match right", true, func(in *s3.CopyObjectInput, _ string) { in.CopySourceIfMatch = aws.String(quote(srcETag)) })
		copyCase("copy source if-match wrong", true, func(in *s3.CopyObjectInput, _ string) { in.CopySourceIfMatch = aws.String(wrongETag) })
		copyCase("copy source if-none-match right", true, func(in *s3.CopyObjectInput, _ string) {
			in.CopySourceIfNoneMatch = aws.String(quote(srcETag))
		})

		// DeleteObject.
		delCase := func(name string, set func(in *s3.DeleteObjectInput, etag string)) {
			key, before, etag, ok := fresh(true)
			if !ok {
				return
			}
			in := &s3.DeleteObjectInput{Bucket: &r.bucket, Key: &key}
			set(in, etag)
			out, err := r.w.DeleteObject(ctx, in)
			var md middleware.Metadata
			if out != nil {
				md = out.ResultMetadata
			}
			outcome := state(key, before, nil)
			if outcome == "unchanged" {
				outcome = "still there"
			} else if outcome == "absent" {
				outcome = "deleted"
			}
			rec(name, code(md, err), outcome)
		}
		delCase("delete if-match right", func(in *s3.DeleteObjectInput, e string) { in.IfMatch = aws.String(quote(e)) })
		delCase("delete if-match wrong", func(in *s3.DeleteObjectInput, _ string) { in.IfMatch = aws.String(wrongETag) })

		// GetObject and HeadObject. Wait so that "last modified + 1s" lies in the past.
		key, body, etag, ok := fresh(true)
		if !ok {
			continue
		}
		h, err := r.w.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &r.bucket, Key: &key})
		if r.failed("head", err) {
			continue
		}
		mod := aws.ToTime(h.LastModified)
		time.Sleep(3 * time.Second)
		type cond struct {
			name                     string
			ifMatch, ifNoneMatch     *string
			ifModSince, ifUnmodSince *time.Time
		}
		conds := []cond{
			{name: "if-match right", ifMatch: aws.String(quote(etag))},
			{name: "if-match wrong", ifMatch: aws.String(wrongETag)},
			{name: "if-none-match right", ifNoneMatch: aws.String(quote(etag))},
			{name: "if-none-match wrong", ifNoneMatch: aws.String(wrongETag)},
			{name: "if-none-match *", ifNoneMatch: aws.String("*")},
			{name: "if-modified-since 1h before", ifModSince: aws.Time(mod.Add(-time.Hour))},
			{name: "if-modified-since 1s after", ifModSince: aws.Time(mod.Add(time.Second))},
			{name: "if-unmodified-since 1h before", ifUnmodSince: aws.Time(mod.Add(-time.Hour))},
			{name: "if-unmodified-since 1s after", ifUnmodSince: aws.Time(mod.Add(time.Second))},
		}
		for _, c := range conds {
			g, err := r.r.GetObject(ctx, &s3.GetObjectInput{Bucket: &r.bucket, Key: &key, IfMatch: c.ifMatch,
				IfNoneMatch: c.ifNoneMatch, IfModifiedSince: c.ifModSince, IfUnmodifiedSince: c.ifUnmodSince})
			outcome := "no body"
			var md middleware.Metadata
			if g != nil {
				md = g.ResultMetadata
				b, rerr := readAll(g.Body)
				switch {
				case rerr != nil:
					outcome = "body read error"
				case bytes.Equal(b, body):
					outcome = "body returned"
				default:
					outcome = "wrong body"
				}
			}
			rec("get "+c.name, code(md, err), outcome)
			hd, err := r.r.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &r.bucket, Key: &key, IfMatch: c.ifMatch,
				IfNoneMatch: c.ifNoneMatch, IfModifiedSince: c.ifModSince, IfUnmodifiedSince: c.ifUnmodSince})
			md = middleware.Metadata{}
			if hd != nil {
				md = hd.ResultMetadata
			}
			rec("head "+c.name, code(md, err), "-")
		}
	}
	r.note("conditional_full", res)
	for name, m := range res {
		fmt.Printf("COND %-36s %v\n", name, m)
	}
}

// abortUploads aborts every multipart upload left open under the prefix.
func (r *runner) abortUploads(ctx context.Context) {
	out, err := r.w.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: &r.bucket, Prefix: &r.prefix})
	if r.failed("list_uploads", err) {
		return
	}
	for _, u := range out.Uploads {
		_, err := r.w.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &r.bucket, Key: u.Key, UploadId: u.UploadId})
		r.failed("abort_upload", err)
	}
	r.note("open_uploads_at_cleanup", len(out.Uploads))
}

func readAll(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(rc)
}
