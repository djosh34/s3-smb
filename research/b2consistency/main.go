// Command b2consistency checks whether a real B2 bucket gives the read-after-write
// and LIST consistency the new engine needs (issue #604). It writes only under a
// fresh prefix, counts every anomaly with its timing, and deletes every object
// version it made at the end. It reads B2_KEY_ID, B2_APPLICATION_KEY,
// B2_ENDPOINT and B2_BUCKET from the environment and never prints the keys.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

const (
	budget      = 45000 // stop starting new tests past this many HTTP attempts
	settlePoll  = 100 * time.Millisecond
	settleLimit = 20 * time.Second
)

type anomaly struct {
	Test      string `json:"test"`
	Check     string `json:"check"`
	Key       string `json:"key"`
	AfterMs   int64  `json:"after_write_ms"`
	SettledMs int64  `json:"settled_ms"` // -1 if it never settled within settleLimit
	Detail    string `json:"detail"`
}

type runner struct {
	w, r     *s3.Client // writer and reader use separate connection pools
	bucket   string
	prefix   string
	attempts atomic.Int64

	mu        sync.Mutex
	checks    map[string]int
	anomalies []anomaly
	errs      map[string]int
	errSample map[string]string
	lat       map[string][]time.Duration
	notes     map[string]any
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	id, key := os.Getenv("B2_KEY_ID"), os.Getenv("B2_APPLICATION_KEY")
	endpoint, bucket := os.Getenv("B2_ENDPOINT"), os.Getenv("B2_BUCKET")
	if id == "" || key == "" || endpoint == "" || bucket == "" {
		return errors.New("B2_KEY_ID, B2_APPLICATION_KEY, B2_ENDPOINT and B2_BUCKET must be set")
	}
	region := os.Getenv("B2_REGION")
	if region == "" {
		host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
		parts := strings.Split(host, ".")
		if len(parts) < 2 {
			return fmt.Errorf("cannot read the region from endpoint %q", endpoint)
		}
		region = parts[1]
	}
	r := &runner{
		bucket: bucket, checks: map[string]int{}, errs: map[string]int{},
		anomalies: []anomaly{}, errSample: map[string]string{}, lat: map[string][]time.Duration{}, notes: map[string]any{},
	}
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(id, key, "")),
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
		config.WithRetryMaxAttempts(5),
		config.WithAPIOptions([]func(*middleware.Stack) error{r.countAttempts}),
	)
	if err != nil {
		return err
	}
	newClient := func() *s3.Client {
		return s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
			o.HTTPClient = awshttp.NewBuildableClient()
		})
	}
	r.w, r.r = newClient(), newClient()
	r.prefix = fmt.Sprintf("consistency/%s-%s/", time.Now().UTC().Format("20060102T150405Z"), randHex(4))
	fmt.Printf("bucket %s, region %s, prefix %s\n", bucket, region, r.prefix)

	start := time.Now()
	tests := []struct {
		name string
		fn   func(context.Context)
	}{
		{"bucket", r.testBucket},
		{"raw_small", r.testRawSmall},
		{"raw_8mib", r.testRawLarge},
		{"newest_copy", r.testNewestCopy},
		{"late_copy", r.testLateCopy},
		{"delete", r.testDelete},
		{"delete_overwritten", r.testDeleteOverwritten},
		{"overwrite", r.testOverwrite},
		{"parallel_distinct", r.testParallelDistinct},
		{"parallel_same_key", r.testParallelSameKey},
		{"if_none_match", r.testConditional},
		{"versions_paging", r.testVersionsPaging},
		{"conditional_full", r.testConditionalFull},
	}
	durations := map[string]string{}
	only := os.Getenv("B2C_ONLY")
	for _, t := range tests {
		if only != "" && !slices.Contains(strings.Split(only, ","), t.name) {
			continue
		}
		if r.attempts.Load() > budget {
			fmt.Printf("skip %s: request budget reached\n", t.name)
			r.note("skipped_"+t.name, true)
			continue
		}
		t0, a0 := time.Now(), r.attempts.Load()
		t.fn(ctx)
		durations[t.name] = time.Since(t0).Round(time.Millisecond).String()
		fmt.Printf("%-20s %8s %6d requests, %d anomalies so far\n", t.name, durations[t.name], r.attempts.Load()-a0, len(r.anomalies))
	}
	testRequests := r.attempts.Load()
	r.hiddenVersions(ctx)
	leftover := r.cleanup(ctx)

	out := map[string]any{
		"prefix": r.prefix, "region": region, "duration": time.Since(start).Round(time.Second).String(),
		"test_durations": durations, "requests_tests": testRequests, "requests_total": r.attempts.Load(),
		"checks": r.checks, "anomaly_counts": r.anomalyCounts(), "anomalies": r.anomalies,
		"request_errors": r.errs, "request_error_samples": r.errSample, "latency": r.latency(),
		"notes": r.notes, "leftover_versions": leftover,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile("results.json", b, 0o644); err != nil {
		return err
	}
	fmt.Println(string(b))
	total := 0
	for _, n := range r.checks {
		total += n
	}
	if only == "" && total < 1000 {
		return fmt.Errorf("only %d checks ran; see request_errors", total)
	}
	if leftover != 0 {
		return fmt.Errorf("%d object versions were left under %s", leftover, r.prefix)
	}
	return nil
}

// countAttempts counts every HTTP attempt, retries included.
func (r *runner) countAttempts(stack *middleware.Stack) error {
	return stack.Finalize.Add(middleware.FinalizeMiddlewareFunc("countAttempts",
		func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
			r.attempts.Add(1)
			return next.HandleFinalize(ctx, in)
		}), middleware.After)
}

// ---- small helpers ----

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func randBody(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:8]) }

func trimETag(s *string) string { return strings.Trim(aws.ToString(s), `"`) }

func is404(err error) bool {
	var re *awshttp.ResponseError
	return errors.As(err, &re) && re.HTTPStatusCode() == 404
}

func status(err error) string {
	if err == nil {
		return "200"
	}
	code := "?"
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		code = fmt.Sprint(re.HTTPStatusCode())
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code += " " + ae.ErrorCode()
	}
	return code
}

func forEach(n, workers int, fn func(i int)) {
	var wg sync.WaitGroup
	next := atomic.Int64{}
	for range workers {
		wg.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				fn(i)
			}
		})
	}
	wg.Wait()
}

func (r *runner) note(k string, v any) { r.mu.Lock(); r.notes[k] = v; r.mu.Unlock() }

func (r *runner) addNote(k string, d int) {
	r.mu.Lock()
	n, _ := r.notes[k].(int)
	r.notes[k] = n + d
	r.mu.Unlock()
}

func (r *runner) timed(op string, t0 time.Time) {
	d := time.Since(t0)
	r.mu.Lock()
	r.lat[op] = append(r.lat[op], d)
	r.mu.Unlock()
}

func (r *runner) failed(op string, err error) bool {
	if err == nil {
		return false
	}
	r.mu.Lock()
	k := op + " " + status(err)
	r.errs[k]++
	if _, ok := r.errSample[k]; !ok {
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		r.errSample[k] = msg
	}
	r.mu.Unlock()
	return true
}

// check records one consistency check. When it fails, it polls settle until it
// holds, so the report shows how long the anomaly lasted.
func (r *runner) check(test, name, key string, written time.Time, ok bool, detail string, settle func() bool) {
	r.mu.Lock()
	r.checks[test+"/"+name]++
	r.mu.Unlock()
	if ok {
		return
	}
	a := anomaly{Test: test, Check: name, Key: strings.TrimPrefix(key, r.prefix),
		AfterMs: time.Since(written).Milliseconds(), SettledMs: -1, Detail: detail}
	fmt.Printf("ANOMALY %s/%s %s after %dms: %s\n", test, name, a.Key, a.AfterMs, detail)
	if settle != nil {
		for time.Since(written) < settleLimit {
			time.Sleep(settlePoll)
			if settle() {
				a.SettledMs = time.Since(written).Milliseconds()
				break
			}
		}
	}
	r.mu.Lock()
	r.anomalies = append(r.anomalies, a)
	r.mu.Unlock()
}

func (r *runner) anomalyCounts() map[string]int {
	m := map[string]int{}
	for _, a := range r.anomalies {
		m[a.Test+"/"+a.Check]++
	}
	return m
}

func (r *runner) latency() map[string]map[string]string {
	out := map[string]map[string]string{}
	for op, ds := range r.lat {
		slices.Sort(ds)
		p := func(q float64) string { return ds[int(q*float64(len(ds)-1))].Round(time.Millisecond).String() }
		out[op] = map[string]string{"n": fmt.Sprint(len(ds)), "p50": p(0.5), "p99": p(0.99), "max": p(1)}
	}
	return out
}

// ---- S3 operations; reads go through the reader client ----

func (r *runner) put(ctx context.Context, key string, body []byte) (string, error) {
	t0 := time.Now()
	out, err := r.w.PutObject(ctx, &s3.PutObjectInput{Bucket: &r.bucket, Key: &key,
		Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))})
	r.timed("put", t0)
	if r.failed("put", err) {
		return "", err
	}
	return trimETag(out.ETag), nil
}

// get returns found=false on 404. err is set only for other failures.
func (r *runner) get(ctx context.Context, key string) ([]byte, bool, error) {
	t0 := time.Now()
	out, err := r.r.GetObject(ctx, &s3.GetObjectInput{Bucket: &r.bucket, Key: &key})
	if is404(err) {
		r.timed("get", t0)
		return nil, false, nil
	}
	if r.failed("get", err) {
		return nil, false, err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	r.timed("get", t0)
	if r.failed("get_body", err) {
		return nil, false, err
	}
	return b, true, nil
}

type headResult struct {
	found bool
	size  int64
	etag  string
}

func (r *runner) head(ctx context.Context, key string) (headResult, error) {
	t0 := time.Now()
	out, err := r.r.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &r.bucket, Key: &key})
	r.timed("head", t0)
	if is404(err) {
		return headResult{}, nil
	}
	if r.failed("head", err) {
		return headResult{}, err
	}
	return headResult{true, aws.ToInt64(out.ContentLength), trimETag(out.ETag)}, nil
}

type listed struct {
	size     int64
	etag     string
	modified time.Time
}

// list returns every current key under prefix and how many entries each key had.
func (r *runner) list(ctx context.Context, prefix string) (map[string]listed, map[string]int, error) {
	objs, counts := map[string]listed{}, map[string]int{}
	p := s3.NewListObjectsV2Paginator(r.r, &s3.ListObjectsV2Input{Bucket: &r.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		t0 := time.Now()
		page, err := p.NextPage(ctx)
		r.timed("list", t0)
		if r.failed("list", err) {
			return nil, nil, err
		}
		for _, o := range page.Contents {
			k := aws.ToString(o.Key)
			counts[k]++
			objs[k] = listed{aws.ToInt64(o.Size), trimETag(o.ETag), aws.ToTime(o.LastModified)}
		}
	}
	return objs, counts, nil
}

func (r *runner) del(ctx context.Context, key string) error {
	t0 := time.Now()
	_, err := r.w.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &r.bucket, Key: &key})
	r.timed("delete", t0)
	r.failed("delete", err)
	return err
}

// multipart uploads body in parts of partSize, leaving the upload open until
// the returned complete function runs.
func (r *runner) multipart(ctx context.Context, key string, body []byte, partSize int) (func() error, error) {
	t0 := time.Now()
	cr, err := r.w.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &r.bucket, Key: &key})
	if r.failed("mpu_create", err) {
		return nil, err
	}
	var parts []types.CompletedPart
	for n, off := int32(1), 0; off < len(body); n, off = n+1, off+partSize {
		end := min(off+partSize, len(body))
		up, err := r.w.UploadPart(ctx, &s3.UploadPartInput{Bucket: &r.bucket, Key: &key, UploadId: cr.UploadId,
			PartNumber: aws.Int32(n), Body: bytes.NewReader(body[off:end]), ContentLength: aws.Int64(int64(end - off))})
		if r.failed("mpu_part", err) {
			return nil, err
		}
		parts = append(parts, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(n)})
	}
	return func() error {
		_, err := r.w.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &r.bucket, Key: &key,
			UploadId: cr.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
		r.timed("mpu_total", t0)
		r.failed("mpu_complete", err)
		return err
	}, nil
}

// ---- read-after-write checks shared by several tests ----

// verifyPresent checks GET, HEAD and LIST right after a write of body to key.
// The order rotates so that each read is sometimes the first one after the write.
func (r *runner) verifyPresent(ctx context.Context, test, key string, body []byte, etag string, written time.Time, order int) {
	want := sum(body)
	getOK := func() (bool, string) {
		b, found, err := r.get(ctx, key)
		if err != nil {
			return true, "" // a request error is counted, not an anomaly
		}
		if !found {
			return false, "GET 404"
		}
		if sum(b) != want {
			return false, fmt.Sprintf("GET body %s, want %s", sum(b), want)
		}
		return true, ""
	}
	headOK := func() (bool, string) {
		h, err := r.head(ctx, key)
		if err != nil {
			return true, ""
		}
		if !h.found {
			return false, "HEAD 404"
		}
		if h.size != int64(len(body)) || (etag != "" && h.etag != etag) {
			return false, fmt.Sprintf("HEAD size %d etag %s, want %d %s", h.size, h.etag, len(body), etag)
		}
		return true, ""
	}
	listOK := func() (bool, string) {
		objs, counts, err := r.list(ctx, key)
		if err != nil {
			return true, ""
		}
		o, found := objs[key]
		if !found {
			return false, "LIST missing"
		}
		if counts[key] != 1 {
			return false, fmt.Sprintf("LIST shows the key %d times", counts[key])
		}
		if o.size != int64(len(body)) || (etag != "" && o.etag != etag) {
			return false, fmt.Sprintf("LIST size %d etag %s, want %d %s", o.size, o.etag, len(body), etag)
		}
		return true, ""
	}
	type c struct {
		name string
		fn   func() (bool, string)
	}
	cs := []c{{"get", getOK}, {"head", headOK}, {"list", listOK}}
	for i := range cs {
		c := cs[(i+order)%len(cs)]
		ok, d := c.fn()
		r.check(test, c.name, key, written, ok, d, func() bool { ok, _ := c.fn(); return ok })
	}
}

func (r *runner) verifyGone(ctx context.Context, test, key string, deleted time.Time, order int) {
	getOK := func() (bool, string) {
		b, found, err := r.get(ctx, key)
		if err != nil || !found {
			return true, ""
		}
		return false, fmt.Sprintf("GET found %d bytes %s", len(b), sum(b))
	}
	headOK := func() (bool, string) {
		h, err := r.head(ctx, key)
		if err != nil || !h.found {
			return true, ""
		}
		return false, fmt.Sprintf("HEAD found size %d", h.size)
	}
	listOK := func() (bool, string) {
		objs, _, err := r.list(ctx, key)
		if err != nil {
			return true, ""
		}
		if o, found := objs[key]; found {
			return false, fmt.Sprintf("LIST shows size %d", o.size)
		}
		return true, ""
	}
	type c struct {
		name string
		fn   func() (bool, string)
	}
	cs := []c{{"get_gone", getOK}, {"head_gone", headOK}, {"list_gone", listOK}}
	for i := range cs {
		c := cs[(i+order)%len(cs)]
		ok, d := c.fn()
		r.check(test, c.name, key, deleted, ok, d, func() bool { ok, _ := c.fn(); return ok })
	}
}

// ---- tests ----

func (r *runner) testBucket(ctx context.Context) {
	v, err := r.w.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &r.bucket})
	if err != nil {
		r.note("bucket_versioning", "error "+status(err))
	} else {
		r.note("bucket_versioning", string(v.Status))
	}
	l, err := r.w.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: &r.bucket})
	if err != nil {
		r.note("object_lock", "error "+status(err))
	} else if l.ObjectLockConfiguration != nil {
		r.note("object_lock", string(l.ObjectLockConfiguration.ObjectLockEnabled))
	}
	lc, err := r.w.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: &r.bucket})
	if err != nil {
		r.note("lifecycle", "error "+status(err))
	} else {
		r.note("lifecycle_rules", len(lc.Rules))
	}
}

// PUT a new key, then GET, HEAD and LIST it at once.
func (r *runner) testRawSmall(ctx context.Context) {
	forEach(1500, 8, func(i int) {
		key := fmt.Sprintf("%sraw/%05d", r.prefix, i)
		body := randBody(64 + i%4000)
		etag, err := r.put(ctx, key, body)
		if err != nil {
			return
		}
		r.verifyPresent(ctx, "raw_small", key, body, etag, time.Now(), i)
	})
}

// The same with 8 MiB bodies, the chunk size of the new engine.
func (r *runner) testRawLarge(ctx context.Context) {
	forEach(30, 4, func(i int) {
		key := fmt.Sprintf("%sraw8m/%03d", r.prefix, i)
		body := randBody(8 << 20)
		etag, err := r.put(ctx, key, body)
		if err != nil {
			return
		}
		r.verifyPresent(ctx, "raw_8mib", key, body, etag, time.Now(), i)
	})
}

// The newest database copy rule: copies get increasing numbers, the highest
// name is the newest, and only the last 4 are kept. After every upload a LIST
// must show exactly the expected set, with the new copy as the highest.
func (r *runner) testNewestCopy(ctx context.Context) {
	const keep = 4
	dir := r.prefix + "copies/"
	name := func(n int) string { return fmt.Sprintf("%s%010d-%010d", dir, n, n*7) }
	var live []int
	for n := 1; n <= 600; n++ {
		key := name(n)
		var err error
		if n%25 == 0 {
			// Large copies go up as multipart uploads.
			var complete func() error
			complete, err = r.multipart(ctx, key, randBody(6<<20), 5<<20)
			if err == nil {
				err = complete()
			}
		} else {
			_, err = r.put(ctx, key, randBody(1024+n%16384))
		}
		if err != nil {
			continue
		}
		written := time.Now()
		live = append(live, n)
		want := map[string]bool{}
		for _, m := range live {
			want[name(m)] = true
		}
		highest := func() (bool, string) {
			objs, _, err := r.list(ctx, dir)
			if err != nil {
				return true, ""
			}
			keys := make([]string, 0, len(objs))
			for k := range objs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if len(keys) == 0 || keys[len(keys)-1] != key {
				top := "none"
				if len(keys) > 0 {
					top = strings.TrimPrefix(keys[len(keys)-1], dir)
				}
				return false, "highest is " + top
			}
			var extra, missing []string
			for _, k := range keys {
				if !want[k] {
					extra = append(extra, strings.TrimPrefix(k, dir))
				}
			}
			for k := range want {
				if _, ok := objs[k]; !ok {
					missing = append(missing, strings.TrimPrefix(k, dir))
				}
			}
			if len(extra)+len(missing) > 0 {
				return false, fmt.Sprintf("extra %v missing %v", extra, missing)
			}
			return true, ""
		}
		ok, d := highest()
		r.check("newest_copy", "list_set", key, written, ok, d, func() bool { ok, _ := highest(); return ok })
		if len(live) > keep {
			if r.del(ctx, name(live[0])) == nil {
				live = live[1:]
			}
		}
	}
}

// A late copy: an older copy number finishes its upload after a newer one.
// LIST must show both, and the highest name must still be the newer copy.
// LastModified order is recorded, because the engine must not sort by time.
func (r *runner) testLateCopy(ctx context.Context) {
	forEach(40, 4, func(i int) {
		dir := fmt.Sprintf("%slate/%03d/", r.prefix, i)
		older, newer := dir+"0000000010-0000000100", dir+"0000000011-0000000110"
		var complete func() error
		var err error
		if i%2 == 0 {
			// The older copy is a multipart upload left open while the newer lands.
			complete, err = r.multipart(ctx, older, randBody(256<<10), 5<<20)
			if err != nil {
				return
			}
			if _, err := r.put(ctx, newer, randBody(4096)); err != nil {
				return
			}
			if err := complete(); err != nil {
				return
			}
		} else {
			// A plain PUT of the older copy after the newer one.
			if _, err := r.put(ctx, newer, randBody(4096)); err != nil {
				return
			}
			if _, err := r.put(ctx, older, randBody(4096)); err != nil {
				return
			}
		}
		written := time.Now()
		chk := func() (bool, string) {
			objs, _, err := r.list(ctx, dir)
			if err != nil {
				return true, ""
			}
			_, a := objs[older]
			_, b := objs[newer]
			if !a || !b || len(objs) != 2 {
				return false, fmt.Sprintf("older %v newer %v count %d", a, b, len(objs))
			}
			if objs[older].modified.After(objs[newer].modified) {
				r.addNote("late_copy_older_has_later_lastmodified", 1)
			} else {
				r.addNote("late_copy_older_has_same_or_earlier_lastmodified", 1)
			}
			return true, ""
		}
		ok, d := chk()
		r.check("late_copy", "list_both", newer, written, ok, d, func() bool { ok, _ := chk(); return ok })
	})
}

// PUT, DELETE, then GET, HEAD and LIST must not see the key.
func (r *runner) testDelete(ctx context.Context) {
	forEach(1200, 8, func(i int) {
		key := fmt.Sprintf("%sdel/%05d", r.prefix, i)
		if _, err := r.put(ctx, key, randBody(128+i%2000)); err != nil {
			return
		}
		if r.del(ctx, key) != nil {
			return
		}
		r.verifyGone(ctx, "delete", key, time.Now(), i)
	})
}

// A key with an older hidden version: after DELETE, no read may fall back to
// the older version.
func (r *runner) testDeleteOverwritten(ctx context.Context) {
	n, versionCheck := 300, os.Getenv("B2C_VERSION_CHECK") != ""
	if v, err := strconv.Atoi(os.Getenv("B2C_DELOW_N")); err == nil {
		n = v
	}
	forEach(n, 8, func(i int) {
		key := fmt.Sprintf("%sdelow/%05d", r.prefix, i)
		if _, err := r.put(ctx, key, randBody(100+i)); err != nil {
			return
		}
		if _, err := r.put(ctx, key, randBody(200+i)); err != nil {
			return
		}
		if r.del(ctx, key) != nil {
			return
		}
		deleted := time.Now()
		r.verifyGone(ctx, "delete_overwritten", key, deleted, i)
		if !versionCheck {
			return
		}
		// ListObjectVersions must mark exactly one entry as latest: the marker.
		chk := func() (bool, string) {
			vs, err := r.versions(ctx, key)
			if err != nil {
				return true, ""
			}
			latest, markerLatest := 0, false
			var all []string
			for _, v := range vs {
				if v.key != key {
					continue
				}
				all = append(all, v.String())
				if v.latest {
					latest++
					markerLatest = v.marker
				}
			}
			if latest != 1 || !markerLatest || len(all) != 3 {
				return false, strings.Join(all, "; ")
			}
			return true, ""
		}
		ok, d := chk()
		r.check("delete_overwritten", "versions_latest_is_marker", key, deleted, ok, d, func() bool { ok, _ := chk(); return ok })
	})
}

// Overwrite a key again and again; every read must see the newest body, and
// LIST must show the key once with the newest size.
func (r *runner) testOverwrite(ctx context.Context) {
	forEach(100, 8, func(i int) {
		key := fmt.Sprintf("%sover/%03d", r.prefix, i)
		for round := range 10 {
			body := randBody(500 + round*37 + i)
			etag, err := r.put(ctx, key, body)
			if err != nil {
				continue
			}
			r.verifyPresent(ctx, "overwrite", key, body, etag, time.Now(), i+round)
		}
	})
}

// 50 parallel PUTs to distinct keys, then one LIST must show all of them.
func (r *runner) testParallelDistinct(ctx context.Context) {
	for batch := range 40 {
		dir := fmt.Sprintf("%spar/%02d/", r.prefix, batch)
		sizes := make([]int64, 50)
		var mu sync.Mutex
		ok := map[string]int64{}
		forEach(50, 50, func(i int) {
			key := fmt.Sprintf("%s%02d", dir, i)
			sizes[i] = int64(100 + batch*50 + i)
			if _, err := r.put(ctx, key, randBody(int(sizes[i]))); err == nil {
				mu.Lock()
				ok[key] = sizes[i]
				mu.Unlock()
			}
		})
		written := time.Now()
		chk := func() (bool, string) {
			objs, _, err := r.list(ctx, dir)
			if err != nil {
				return true, ""
			}
			var bad []string
			for k, s := range ok {
				if o, found := objs[k]; !found || o.size != s {
					bad = append(bad, strings.TrimPrefix(k, dir))
				}
			}
			if len(bad) > 0 || len(objs) != len(ok) {
				return false, fmt.Sprintf("listed %d of %d, wrong or missing %v", len(objs), len(ok), bad)
			}
			return true, ""
		}
		good, d := chk()
		r.check("parallel_distinct", "list_all", dir, written, good, d, func() bool { good, _ := chk(); return good })
		for i := range 5 {
			key := fmt.Sprintf("%s%02d", dir, (batch*7+i*11)%50)
			if s, found := ok[key]; found {
				h, err := r.head(ctx, key)
				r.check("parallel_distinct", "head", key, written, err != nil || (h.found && h.size == s),
					fmt.Sprintf("HEAD found %v size %d want %d", h.found, h.size, s), nil)
			}
		}
	}
}

// 8 parallel PUTs to one key. Afterwards every read must agree on one of the
// bodies. Whether the last PUT to finish wins is recorded.
func (r *runner) testParallelSameKey(ctx context.Context) {
	forEach(50, 2, func(round int) {
		key := fmt.Sprintf("%ssame/%02d", r.prefix, round)
		var mu sync.Mutex
		etags := map[string]time.Time{}
		forEach(8, 8, func(i int) {
			etag, err := r.put(ctx, key, randBody(1000+i))
			if err == nil {
				mu.Lock()
				etags[etag] = time.Now()
				mu.Unlock()
			}
		})
		if len(etags) == 0 {
			return
		}
		written := time.Now()
		var last string
		var lastAt time.Time
		for e, t := range etags {
			if t.After(lastAt) {
				last, lastAt = e, t
			}
		}
		seen := map[string]bool{}
		for range 3 {
			if h, err := r.head(ctx, key); err == nil && h.found {
				seen[h.etag] = true
			}
		}
		if objs, _, err := r.list(ctx, key); err == nil {
			if o, found := objs[key]; found {
				seen[o.etag] = true
			}
		}
		var winner string
		for e := range seen {
			winner = e
		}
		_, known := etags[winner]
		r.check("parallel_same_key", "reads_agree", key, written, len(seen) == 1 && known,
			fmt.Sprintf("reads saw %d etags, known %v", len(seen), known), nil)
		if winner == last {
			r.addNote("same_key_winner_is_last_finished", 1)
		} else {
			r.addNote("same_key_winner_is_not_last_finished", 1)
		}
	})
}

// For the record only: what If-None-Match and If-Match do on PutObject.
func (r *runner) testConditional(ctx context.Context) {
	results := map[string]map[string]int{}
	recS := func(what, outcome string) {
		if results[what] == nil {
			results[what] = map[string]int{}
		}
		results[what][outcome]++
	}
	rec := func(what string, err error) { recS(what, status(err)) }
	for i := range 10 {
		key := fmt.Sprintf("%scond/%02d", r.prefix, i)
		put := func(body []byte, inm, im string) (*s3.PutObjectOutput, error) {
			in := &s3.PutObjectInput{Bucket: &r.bucket, Key: &key, Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))}
			if inm != "" {
				in.IfNoneMatch = aws.String(inm)
			}
			if im != "" {
				in.IfMatch = aws.String(im)
			}
			return r.w.PutObject(ctx, in)
		}
		first := randBody(100)
		_, err := put(first, "*", "")
		rec("if_none_match_new_key", err)
		_, err = put(randBody(101), "*", "")
		rec("if_none_match_existing_key", err)
		b, found, _ := r.get(ctx, key)
		switch {
		case !found:
			recS("existing_key_after_conditional", "missing")
		case bytes.Equal(b, first):
			recS("existing_key_after_conditional", "first body kept")
		default:
			recS("existing_key_after_conditional", "overwritten")
		}
		h, _ := r.head(ctx, key)
		_, err = put(randBody(102), "", `"`+h.etag+`"`)
		rec("if_match_right_etag", err)
		_, err = put(randBody(103), "", `"00000000000000000000000000000000"`)
		rec("if_match_wrong_etag", err)
		_ = r.del(ctx, key)
		_, err = put(randBody(104), "*", "")
		rec("if_none_match_after_delete", err)
	}
	r.note("conditional_put", results)
}

// Does ListObjectVersions mark a version as latest when the key's versions
// are split across pages? Each key gets PUT, PUT, DELETE: 3 entries, of which
// only the marker is latest. Recorded per page size, for the record.
func (r *runner) testVersionsPaging(ctx context.Context) {
	dir := r.prefix + "vpage/"
	for i := range 5 {
		key := fmt.Sprintf("%s%02d", dir, i)
		if _, err := r.put(ctx, key, randBody(100)); err != nil {
			return
		}
		if _, err := r.put(ctx, key, randBody(200)); err != nil {
			return
		}
		if r.del(ctx, key) != nil {
			return
		}
	}
	results := map[string]any{}
	for _, size := range []int32{0, 1, 2, 4} {
		vs, err := r.versionsPaged(ctx, dir, size)
		if err != nil {
			continue
		}
		latest := map[string]int{}
		markerLatest := 0
		for _, v := range vs {
			if v.latest {
				latest[v.key]++
				if v.marker {
					markerLatest++
				}
			}
		}
		wrong, total := 0, 0
		for _, n := range latest {
			total += n
			if n != 1 {
				wrong++
			}
		}
		results[fmt.Sprintf("page_size_%d", size)] = map[string]int{"entries": len(vs),
			"latest_entries": total,
			"latest_markers": markerLatest, "keys_with_not_one_latest": wrong}
	}
	r.note("versions_paging", results)
}

// ---- end checks and cleanup ----

type version struct {
	key, id  string
	marker   bool
	latest   bool
	modified time.Time
}

func (v version) String() string {
	kind := "version"
	if v.marker {
		kind = "marker"
	}
	return fmt.Sprintf("%s latest=%v modified=%s id=%s", kind, v.latest, v.modified.Format("15:04:05.000"), v.id)
}

func (r *runner) versions(ctx context.Context, prefix string) ([]version, error) {
	return r.versionsPaged(ctx, prefix, 0)
}

// versionsPaged lists versions with pageSize entries per page; 0 means the server default.
func (r *runner) versionsPaged(ctx context.Context, prefix string, pageSize int32) ([]version, error) {
	var vs []version
	in := &s3.ListObjectVersionsInput{Bucket: &r.bucket, Prefix: &prefix}
	if pageSize > 0 {
		in.MaxKeys = aws.Int32(pageSize)
	}
	p := s3.NewListObjectVersionsPaginator(r.w, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if r.failed("list_versions", err) {
			return nil, err
		}
		for _, v := range page.Versions {
			vs = append(vs, version{aws.ToString(v.Key), aws.ToString(v.VersionId), false, aws.ToBool(v.IsLatest), aws.ToTime(v.LastModified)})
		}
		for _, m := range page.DeleteMarkers {
			vs = append(vs, version{aws.ToString(m.Key), aws.ToString(m.VersionId), true, aws.ToBool(m.IsLatest), aws.ToTime(m.LastModified)})
		}
	}
	return vs, nil
}

// hiddenVersions compares ListObjectsV2 with ListObjectVersions over the whole
// prefix: ListObjectsV2 must show exactly the keys whose latest version is not
// a delete marker.
func (r *runner) hiddenVersions(ctx context.Context) {
	vs, err := r.versions(ctx, r.prefix)
	if err != nil {
		return
	}
	objs, _, err := r.list(ctx, r.prefix)
	if err != nil {
		return
	}
	want := map[string]bool{}
	nver, nmark, hidden := 0, 0, 0
	byKey := map[string][]version{}
	for _, v := range vs {
		byKey[v.key] = append(byKey[v.key], v)
		if v.marker {
			nmark++
		} else {
			nver++
		}
		if v.latest && !v.marker {
			want[v.key] = true
		}
	}
	for _, list := range byKey {
		if len(list) > 1 {
			hidden++
		}
	}
	var extra, missing []string
	for k := range objs {
		if !want[k] {
			extra = append(extra, strings.TrimPrefix(k, r.prefix))
		}
	}
	for k := range want {
		if _, ok := objs[k]; !ok {
			missing = append(missing, strings.TrimPrefix(k, r.prefix))
		}
	}
	r.note("versions_total", nver)
	r.note("delete_markers_total", nmark)
	r.note("keys_with_hidden_versions", hidden)
	r.note("listobjectsv2_keys", len(objs))
	details := map[string][]string{}
	for _, k := range slices.Concat(extra, missing) {
		if len(details) == 10 {
			break
		}
		for _, v := range byKey[r.prefix+k] {
			details[k] = append(details[k], v.String())
		}
	}
	if len(details) > 0 {
		r.note("hidden_versions_mismatch_details", details)
	}
	if len(extra) > 10 {
		extra = extra[:10]
	}
	if len(missing) > 10 {
		missing = missing[:10]
	}
	r.check("hidden_versions", "listv2_matches_latest", r.prefix, time.Now(), len(extra)+len(missing) == 0,
		fmt.Sprintf("extra %v missing %v", extra, missing), nil)
}

// cleanup deletes every version and delete marker under the prefix and
// returns how many are left.
func (r *runner) cleanup(ctx context.Context) int {
	r.abortUploads(ctx)
	for attempt := range 3 {
		vs, err := r.versions(ctx, r.prefix)
		if err != nil {
			continue
		}
		if len(vs) == 0 {
			return 0
		}
		fmt.Printf("cleanup pass %d: %d versions\n", attempt+1, len(vs))
		for start := 0; start < len(vs); start += 1000 {
			batch := vs[start:min(start+1000, len(vs))]
			ids := make([]types.ObjectIdentifier, len(batch))
			for i, v := range batch {
				ids[i] = types.ObjectIdentifier{Key: aws.String(v.key), VersionId: aws.String(v.id)}
			}
			out, err := r.w.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &r.bucket,
				Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)}})
			if err == nil && len(out.Errors) == 0 {
				continue
			}
			r.failed("delete_objects", err)
			r.note("cleanup_used_single_deletes", true)
			forEach(len(batch), 16, func(i int) {
				_, err := r.w.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &r.bucket,
					Key: aws.String(batch[i].key), VersionId: aws.String(batch[i].id)})
				r.failed("delete_version", err)
			})
		}
	}
	vs, err := r.versions(ctx, r.prefix)
	if err != nil {
		return -1
	}
	return len(vs)
}
