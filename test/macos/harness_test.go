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

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

type harness struct {
	t                                                                         *testing.T
	ctx                                                                       context.Context
	work, evidence, transfer, bin, local, share, proof, interval, destination string
	launchdPlist                                                              string
	daemon, minio, backup                                                     *process
	attachments                                                               []string
	serial, applicationSerial                                                 int
	finished                                                                  bool
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) save(name string, value any) {
	h.t.Helper()
	file, err := os.OpenFile(filepath.Join(h.evidence, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // Only fixed evidence names are created under the run-owned directory.
	h.must(err)
	h.must(errors.Join(json.NewEncoder(file).Encode(value), file.Close()))
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
	t.Setenv("MINIO_ROOT_USER", "mac-acceptance")
	t.Setenv("MINIO_ROOT_PASSWORD", "synthetic-mac-acceptance-secret")
	phase := os.Getenv("MAC_PHASE")
	budgets := map[string]time.Duration{"discover": 15 * time.Minute, "backup": 130 * time.Minute, "recover": 70 * time.Minute, "scenario": 100 * time.Minute}
	budget, ok := budgets[phase]
	if !ok {
		t.Fatal("MAC_PHASE must be discover, backup, recover or scenario")
	}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	h := &harness{t: t, ctx: ctx, interval: "5m"}
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
		outcome = h.scenario(os.Getenv("MAC_SCENARIO"))
	}
	h.finish()
	if t.Failed() {
		return
	}
	if phase == "backup" || (phase == "scenario" && os.Getenv("MAC_SCENARIO") == "machine-loss") {
		if h.backup != nil || len(h.attachments) != 0 || h.daemon != nil || h.minio != nil {
			t.Fatal("cannot export active storage")
		}
		h.run(2*time.Minute, "/usr/bin/du", "-sk", filepath.Join(h.work, "objects"))
		h.run(30*time.Minute, "/usr/bin/tar", "-C", h.work, "-cf", filepath.Join(h.transfer, "store.tar"), "objects")
		data, err := json.Marshal(outcome)
		h.must(err)
		h.must(os.WriteFile(filepath.Join(h.transfer, "reference/recovery.json"), data, 0o600)) //nolint:gosec // Only the run-owned transfer path is used; recovery fields are file content.
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

func (h *harness) services(fresh bool) {
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
	if fresh {
		h.run(2*time.Minute, filepath.Join(h.bin, "fixture"), "bucket-create", "--endpoint", "http://127.0.0.1:19000", "--bucket", "time-machine")
		if len(h.objects("")) != 0 {
			h.t.Fatal("initial bucket is not empty")
		}
	}
}

func (h *harness) objects(prefix string) map[string]int64 {
	output := h.run(5*time.Minute, filepath.Join(h.bin, "fixture"), "bucket-list", "--endpoint", "http://127.0.0.1:19000", "--bucket", "time-machine", "--prefix", prefix)
	var result struct {
		Objects []struct {
			Key  string `json:"key"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	h.must(json.Unmarshal([]byte(output), &result))
	objects := make(map[string]int64, len(result.Objects))
	for _, object := range result.Objects {
		objects[object.Key] = object.Size
	}
	return objects
}

func (h *harness) mount() {
	h.must(os.MkdirAll(h.share, 0o700))
	h.run(2*time.Minute, "/sbin/mount_smbfs", "-N", "//timemachine:synthetic-tm-control@127.0.0.1:1445/TimeMachine", h.share)
	h.run(2*time.Minute, "/usr/bin/smbutil", "statshares", "-a")
	h.run(2*time.Minute, "/sbin/mount")
}

func (h *harness) createTree() {
	h.must(absent(h.proof))
	for _, path := range []string{"nested/deeper", "empty", "nested/empty"} {
		h.must(os.MkdirAll(filepath.Join(h.proof, path), 0o700))
	}
	h.randomFile("original.bin", 4_000_000)
	h.must(os.WriteFile(filepath.Join(h.proof, "nested/message.txt"), []byte("independent baseline contents\n"), 0o600))
	h.must(os.WriteFile(filepath.Join(h.proof, "nested/deeper/zero-length"), nil, 0o600))
	h.must(os.Mkdir(filepath.Join(h.transfer, "reference"), 0o700))
	h.manifest(h.proof, filepath.Join(h.transfer, "reference/tree.json"))
}

func (h *harness) randomFile(name string, size int64) {
	file, err := os.OpenFile(filepath.Join(h.proof, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // The name is one of the fixed test fixture files.
	h.must(err)
	_, err = io.CopyN(file, rand.Reader, size)
	h.must(errors.Join(err, file.Close()))
}

func (h *harness) manifest(tree, path string) helpers.Counts {
	rows, counts, err := helpers.Manifest(tree)
	h.must(err)
	h.must(helpers.WriteManifest(path, rows))
	return counts
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
	h.must(os.Mkdir(filepath.Join(h.work, "objects"), 0o700))
	h.exclusions(true)
	for _, name := range []string{"Applications", "Library", "opt"} {
		for _, parent := range []string{"/", "/System/Volumes/Data"} {
			h.t.Log(h.run(2*time.Minute, "/usr/bin/tmutil", "isexcluded", filepath.Join(parent, name)))
		}
	}
	h.checkExclusions()
}
