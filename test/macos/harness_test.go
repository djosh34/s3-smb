//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/test/macos/helpers"
)

const (
	minioUser     = "mac-acceptance"
	minioPassword = "synthetic-mac-acceptance-secret"
	minioEndpoint = "http://127.0.0.1:19000"
)

type harness struct {
	t   *testing.T
	ctx context.Context
	// The run's directories. The roots keep file access inside them.
	work, evidence, transfer, bin, local, share, proof string
	workDir, evidenceDir, transferDir, proofDir        *os.Root
	destination, launchdPlist, smbAddress              string
	// smbLogLevel is the kernel SMB log level to restore, if it was changed.
	smbLogLevel string
	// The bucket that s3-smb uses: MinIO, or B2 when b2 is set.
	endpoint, bucket, region string
	// proxy cuts SMB connections, s3Proxy the connections to MinIO.
	proxy, s3Proxy        *netfault.Proxy
	s3                    *s3.Client
	daemon, minio, backup *process
	backupDirectory       *os.Root
	attachments           []string
	// prepare adds to the test tree before the first backup.
	prepare func()
	// uploaded holds every chunk object listed in this run, with its size.
	uploaded                  map[string]int64
	samples                   []storageSample
	serial, applicationSerial int
	finished, b2              bool
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

// openRoot opens one of the run's directories. It closes after finish.
func (h *harness) openRoot(path string) *os.Root {
	root, err := os.OpenRoot(path)
	h.must(err)
	h.t.Cleanup(func() {
		if err := root.Close(); err != nil {
			h.t.Error(err)
		}
	})
	return root
}

// writeJSON creates name in dir. It never replaces a file.
func writeJSON(dir *os.Root, name string, value any) error {
	file, err := dir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return errors.Join(json.NewEncoder(file).Encode(value), file.Close())
}

func readJSON(dir *os.Root, name string, value any) error {
	data, err := dir.ReadFile(name)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// save writes evidence.
func (h *harness) save(name string, value any) {
	h.t.Helper()
	h.must(writeJSON(h.evidenceDir, name, value))
}

func absent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("path must be absent: %s", path)
}

func TestTimeMachine(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Geteuid() != 0 {
		t.Fatal("native Darwin administrative execution required")
	}
	t.Setenv("MINIO_ROOT_USER", minioUser)
	t.Setenv("MINIO_ROOT_PASSWORD", minioPassword)
	phase := os.Getenv("MAC_PHASE")
	// A start with a new data folder may wait 10 minutes for the old
	// server's stale lock.
	budgets := map[string]time.Duration{"discover": 15 * time.Minute, "backup": 130 * time.Minute, "recover": 85 * time.Minute, "scenario": 140 * time.Minute}
	budget, ok := budgets[phase]
	if !ok {
		t.Fatal("MAC_PHASE must be discover, backup, recover or scenario")
	}
	scenario := os.Getenv("MAC_SCENARIO")
	if phase == "scenario" && scenario == "large" {
		budget = 335 * time.Minute
	}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	h := &harness{t: t, ctx: ctx, smbAddress: "127.0.0.1:1445", endpoint: minioEndpoint, bucket: "time-machine", region: "us-east-1", uploaded: map[string]int64{}}
	if phase == "scenario" && scenario == "b2" {
		h.useB2()
	}
	for name, target := range map[string]*string{"MAC_WORK": &h.work, "MAC_ARTIFACTS": &h.evidence, "MAC_TRANSFER": &h.transfer} {
		*target = os.Getenv(name)
		if !filepath.IsAbs(*target) || filepath.Clean(*target) == "/" {
			t.Fatalf("%s must be an absolute task path", name)
		}
	}
	home := os.Getenv("MAC_RUNNER_HOME")
	if !filepath.IsAbs(home) {
		t.Fatal("MAC_RUNNER_HOME required")
	}
	h.bin, h.local, h.share, h.proof = filepath.Join(h.work, "bin"), filepath.Join(h.work, "daemon"), filepath.Join(h.work, "smb"), filepath.Join(home, "s3-smb-acceptance-proof")
	h.must(absent(h.work))
	h.must(absent(h.evidence))
	h.must(os.Mkdir(h.work, 0o700))
	h.must(os.Mkdir(h.evidence, 0o700))
	if phase != "recover" {
		h.must(absent(h.transfer))
		h.must(os.Mkdir(h.transfer, 0o700))
	}
	// Cleanups run in reverse order, so finish runs before these roots close.
	h.workDir, h.evidenceDir, h.transferDir = h.openRoot(h.work), h.openRoot(h.evidence), h.openRoot(h.transfer)
	t.Cleanup(h.finish)
	h.build()
	var outcome result
	switch phase {
	case "discover":
		h.discover()
	case "recover":
		outcome = h.recoverStore()
	case "backup":
		outcome = h.baseline()
	case "scenario":
		outcome = h.scenario(scenario)
	}
	h.finish()
	if t.Failed() {
		return
	}
	if phase == "backup" || (phase == "scenario" && scenario == "machine-loss") {
		if h.backup != nil || len(h.attachments) != 0 || h.daemon != nil || h.minio != nil {
			t.Fatal("cannot export active storage")
		}
		h.run(2*time.Minute, "/usr/bin/du", "-sk", filepath.Join(h.work, "objects"))
		h.run(30*time.Minute, "/usr/bin/tar", "-C", h.work, "-cf", filepath.Join(h.transfer, "store.tar"), "objects")
		h.must(writeJSON(h.transferDir, "reference/recovery.json", outcome))
	}
	if outcome.NetworkDrop != nil && outcome.NetworkDrop.Status == "not tested" {
		t.Fatal("not tested: macOS refused reconnect in all three connection-drop attempts")
	}
	h.t.Log("acceptance-passed", outcome)
}

func (h *harness) waitFor(what string, limit, every time.Duration, condition func() (bool, error)) error {
	deadline := time.Now().Add(limit)
	for {
		if err := h.ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s exceeded %s: %w", what, limit, context.DeadlineExceeded)
		}
		if !h.finished && h.daemon != nil && h.daemon.exited() {
			return errors.Join(errors.New("application exited unexpectedly"), h.daemon.err)
		}
		done, err := condition()
		if err != nil || done {
			return err
		}
		select {
		case <-h.ctx.Done():
			return h.ctx.Err()
		case <-time.After(every):
		}
	}
}

func (h *harness) pause(delay time.Duration) {
	select {
	case <-h.ctx.Done():
		h.t.Fatal(h.ctx.Err())
	case <-time.After(delay):
	}
	if h.daemon != nil && h.daemon.exited() {
		h.t.Fatalf("application exited unexpectedly: %v", h.daemon.err)
	}
}

func (h *harness) platform(recovery bool) {
	h.status()
	h.run(2*time.Minute, "/sbin/mount")
	if _, err := h.try(2*time.Minute, "/bin/launchctl", "print", "system/com.apple.backupd"); err != nil {
		h.run(2*time.Minute, "/bin/launchctl", "enable", "system/com.apple.backupd")
		h.run(2*time.Minute, "/bin/launchctl", "bootstrap", "system", "/System/Library/LaunchDaemons/com.apple.backupd.plist")
	}
	h.run(2*time.Minute, "/bin/launchctl", "print", "system/com.apple.backupd")
	for _, path := range []string{h.work, h.evidence, h.transfer} {
		h.run(2*time.Minute, "/usr/bin/tmutil", "addexclusion", "-p", path)
	}
	if !recovery {
		h.exclusions(false)
	}
	h.run(2*time.Minute, "/bin/df", "-k")
}

func (h *harness) exclusions(diagnostic bool) {
	for _, path := range helpers.Exclusions {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			h.t.Log("exclusion-absent", path)
			continue
		} else {
			h.must(err)
		}
		output, err := h.try(2*time.Minute, "/usr/bin/tmutil", "addexclusion", "-p", path)
		h.t.Log("exclusion-added", path, output, err)
		if !diagnostic {
			h.must(err)
		}
	}
}

func (h *harness) checkExclusions() {
	for path, excluded := range map[string]bool{h.proof: false, filepath.Join(h.proof, "nested/message.txt"): false, filepath.Join(h.proof, "empty"): false, filepath.Join(h.work, "objects"): true, "/Users/runner/Library": true} {
		output := h.run(2*time.Minute, "/usr/bin/tmutil", "isexcluded", path)
		h.t.Log(output)
		h.must(helpers.CheckExclusion(output, excluded))
	}
}

// services starts MinIO and connects to it, or connects to B2 in the b2
// scenario. A fresh bucket must be empty.
func (h *harness) services(fresh bool) {
	if h.b2 {
		h.connectB2()
		return
	}
	for _, port := range []string{"1445", "19000", "19003"} {
		listener, err := (&net.ListenConfig{}).Listen(h.ctx, "tcp", "127.0.0.1:"+port)
		h.must(err)
		h.must(listener.Close())
	}
	h.minio = h.start("minio", filepath.Join(h.bin, "minio"), "server", "--address", "127.0.0.1:19000", "--console-address", "127.0.0.1:19003", filepath.Join(h.work, "objects"))
	client := &http.Client{Timeout: 2 * time.Second}
	h.must(h.waitFor("MinIO readiness", time.Minute, 200*time.Millisecond, func() (bool, error) {
		if h.minio.exited() {
			return false, errors.Join(errors.New("MinIO exited during startup"), h.minio.err)
		}
		request, err := http.NewRequestWithContext(h.ctx, http.MethodGet, "http://127.0.0.1:19000/minio/health/ready", nil)
		if err != nil {
			return false, err
		}
		response, err := client.Do(request)
		if err != nil {
			h.t.Log("MinIO not ready", err)
			return false, nil
		}
		return response.StatusCode == http.StatusOK, response.Body.Close()
	}))
	h.s3 = s3.New(s3.Options{
		Region:       h.region,
		BaseEndpoint: aws.String(minioEndpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(minioUser, minioPassword, ""),
	})
	if fresh {
		ctx, cancel := context.WithTimeout(h.ctx, 2*time.Minute)
		defer cancel()
		_, err := h.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(h.bucket)})
		h.must(err)
		if len(h.objects("")) != 0 {
			h.t.Fatal("initial bucket is not empty")
		}
	}
}

// objects returns the size of every object in the bucket under prefix. It
// adds the chunk objects it sees to uploaded.
func (h *harness) objects(prefix string) map[string]int64 {
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Minute)
	defer cancel()
	objects := map[string]int64{}
	pages := s3.NewListObjectsV2Paginator(h.s3, &s3.ListObjectsV2Input{Bucket: aws.String(h.bucket), Prefix: aws.String(prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		h.must(err)
		for _, object := range page.Contents {
			key, size := aws.ToString(object.Key), aws.ToInt64(object.Size)
			objects[key] = size
			if strings.HasPrefix(key, "chunks/") {
				h.uploaded[key] = size
			}
		}
	}
	h.t.Log("bucket-objects", prefix, len(objects))
	return objects
}

func (h *harness) mount() {
	h.must(os.MkdirAll(h.share, 0o700))
	h.run(2*time.Minute, "/sbin/mount_smbfs", "-N", "//timemachine:synthetic-tm-control@"+h.smbAddress+"/TimeMachine", h.share)
	h.run(2*time.Minute, "/usr/bin/smbutil", "statshares", "-a")
	h.run(2*time.Minute, "/sbin/mount")
}

// createTree creates the test tree that Time Machine backs up. Its manifest is
// the reference for restores, also on a fresh Mac.
func (h *harness) createTree() {
	h.must(absent(h.proof))
	h.must(os.Mkdir(h.proof, 0o700))
	h.proofDir = h.openRoot(h.proof)
	for _, path := range []string{"nested/deeper", "empty", "nested/empty"} {
		h.must(h.proofDir.MkdirAll(path, 0o700))
	}
	h.randomFile("original.bin", 4_000_000)
	h.must(h.proofDir.WriteFile("nested/message.txt", []byte("independent baseline contents\n"), 0o600))
	h.must(h.proofDir.WriteFile("nested/deeper/zero-length", nil, 0o600))
	if h.prepare != nil {
		h.prepare()
	}
	h.must(h.transferDir.Mkdir("reference", 0o700))
	h.manifest(h.proof, h.transferDir, "reference/tree.json")
}

// reference reads the manifest that createTree saved.
func (h *harness) reference() []helpers.Entry {
	var rows []helpers.Entry
	h.must(readJSON(h.transferDir, "reference/tree.json", &rows))
	return rows
}

func (h *harness) randomFile(name string, size int64) {
	file, err := h.proofDir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	h.must(err)
	_, err = io.CopyN(file, rand.Reader, size)
	h.must(errors.Join(err, file.Close()))
}

// manifest hashes tree and saves the manifest in dir as name.
func (h *harness) manifest(tree string, dir *os.Root, name string) ([]helpers.Entry, helpers.Counts) {
	rows, counts, err := helpers.Manifest(tree)
	h.must(err)
	h.must(writeJSON(dir, name, rows))
	return rows, counts
}

func (h *harness) discover() {
	var found []string
	for parent, kept := range map[string]string{"/System/Volumes/Data": "Users", "/Users": filepath.Base(filepath.Dir(h.proof)), filepath.Dir(h.proof): filepath.Base(h.proof)} {
		entries, err := os.ReadDir(parent)
		h.must(err)
		parentInfo, err := os.Stat(parent)
		h.must(err)
		parentStat, ok := parentInfo.Sys().(*syscall.Stat_t)
		if !ok {
			h.t.Fatal("missing directory device")
		}
		for _, entry := range entries {
			if entry.Name() == kept || !entry.IsDir() {
				continue
			}
			path := filepath.Join(parent, entry.Name())
			if strings.ContainsAny(path, "\r\n") {
				h.t.Logf("discover-skipped %q", path)
				continue
			}
			info, err := entry.Info()
			h.must(err)
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				h.t.Fatal("missing entry device")
			}
			if stat.Dev != parentStat.Dev {
				continue
			}
			found = append(found, path)
		}
	}
	slices.Sort(found)
	notInList, listOnly := helpers.Difference(found, helpers.Exclusions), helpers.Difference(helpers.Exclusions, found)
	h.save("proposed-exclusions.json", map[string][]string{"proposed": found, "not_in_list": notInList, "list_only": listOnly})
	for _, path := range found {
		h.t.Logf("discover-directory %q", path)
	}
	h.t.Logf("discover-not-in-list %q", notInList)
	h.t.Logf("discover-list-only %q", listOnly)
	h.t.Log("discover-image", os.Getenv("ImageOS"), os.Getenv("ImageVersion"))
	h.t.Log(h.run(2*time.Minute, "/usr/bin/sw_vers"))
	h.t.Log(h.run(2*time.Minute, "/sbin/mount"))
	h.createTree()
	h.must(h.workDir.Mkdir("objects", 0o700))
	h.exclusions(true)
	for _, name := range []string{"Applications", "Library", "opt"} {
		for _, parent := range []string{"/", "/System/Volumes/Data"} {
			h.t.Log(h.run(2*time.Minute, "/usr/bin/tmutil", "isexcluded", filepath.Join(parent, name)))
		}
	}
	h.checkExclusions()
}
