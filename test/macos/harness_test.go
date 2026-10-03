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
	"os/exec"
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
	serial, applicationSerial                                                 int
	daemon, minio, backup                                                     *process
	attachments                                                               []string
	finished                                                                  bool
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}
func (h *harness) save(name string, value any, appendLine bool) {
	h.t.Helper()
	data, err := json.Marshal(value)
	h.must(err)
	flags := os.O_CREATE | os.O_WRONLY | os.O_EXCL
	if appendLine {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(filepath.Join(h.evidence, name), flags, 0o600)
	h.must(err)
	_, err = file.Write(append(data, '\n'))
	h.must(errors.Join(err, file.Close()))
}
func (h *harness) event(name string, fields map[string]any) {
	h.t.Helper()
	h.t.Log(name, fields)
	h.save("acceptance.jsonl", map[string]any{"time": time.Now().UTC(), "event": name, "fields": fields}, true)
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
	budgets := map[string]time.Duration{"discover": 20 * time.Minute, "backup": 130 * time.Minute, "recover": 70 * time.Minute, "scenario": 100 * time.Minute}
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
	var result map[string]any
	switch phase {
	case "discover":
		result = h.discover()
	case "recover":
		result = h.recoverStore()
	case "backup":
		result = h.baseline()
	case "scenario":
		result = h.scenario(os.Getenv("MAC_SCENARIO"))
	}
	h.finish()
	if t.Failed() {
		return
	}
	if phase == "backup" || (phase == "scenario" && os.Getenv("MAC_SCENARIO") == "machine-loss") {
		if h.backup != nil || len(h.attachments) != 0 || h.daemon != nil || h.minio != nil {
			t.Fatal("cannot export active storage")
		}
		h.native("/usr/bin/du", "-sk", filepath.Join(h.work, "objects"))
		h.run(30*time.Minute, "/usr/bin/tar", "-C", h.work, "-cf", filepath.Join(h.transfer, "store.tar"), "objects")
		data, err := json.Marshal(result)
		h.must(err)
		h.must(os.WriteFile(filepath.Join(h.transfer, "reference/recovery.json"), data, 0o600))
	}
	h.event("acceptance-passed", result)
}

func (h *harness) platform(recovery bool) {
	h.status()
	h.native("/sbin/mount")
	if _, err := h.command(h.ctx, 2*time.Minute, "", "/bin/launchctl", "print", "system/com.apple.backupd"); err != nil {
		h.native("/bin/launchctl", "enable", "system/com.apple.backupd")
		h.native("/bin/launchctl", "bootstrap", "system", "/System/Library/LaunchDaemons/com.apple.backupd.plist")
	}
	h.native("/bin/launchctl", "print", "system/com.apple.backupd")
	for _, path := range []string{h.work, h.evidence, h.transfer} {
		h.native("/usr/bin/tmutil", "addexclusion", "-p", path)
	}
	if !recovery {
		h.exclusions(false)
	}
	h.native("/bin/df", "-k")
}
func (h *harness) exclusions(diagnostic bool) {
	for _, path := range helpers.Exclusions {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			h.t.Log("exclusion-absent", path)
			continue
		} else {
			h.must(err)
		}
		output, err := h.command(h.ctx, 2*time.Minute, "", "/usr/bin/tmutil", "addexclusion", "-p", path)
		h.t.Log("exclusion-added", path, output, err)
		if !diagnostic {
			h.must(err)
		}
	}
}
func (h *harness) checkExclusions() {
	for path, excluded := range map[string]bool{h.proof: false, filepath.Join(h.proof, "nested/message.txt"): false, filepath.Join(h.proof, "empty"): false, filepath.Join(h.work, "objects"): true, "/Users/runner/Library": true} {
		output := h.native("/usr/bin/tmutil", "isexcluded", path)
		h.t.Log(output)
		h.must(helpers.CheckExclusion(output, excluded))
	}
}
func (h *harness) services(fresh bool) {
	for _, port := range []string{"1445", "19000", "19003"} {
		listener, err := net.Listen("tcp", "127.0.0.1:"+port)
		h.must(err)
		h.must(listener.Close())
	}
	cmd := exec.CommandContext(h.ctx, filepath.Join(h.bin, "minio"), "server", "--address", "127.0.0.1:19000", "--console-address", "127.0.0.1:19003", filepath.Join(h.work, "objects"))
	cmd.Env = append(os.Environ(), "MINIO_ROOT_USER=mac-acceptance", "MINIO_ROOT_PASSWORD=synthetic-mac-acceptance-secret")
	h.minio = h.start("minio", cmd)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(time.Minute)
	for {
		request, err := http.NewRequestWithContext(h.ctx, http.MethodGet, "http://127.0.0.1:19000/minio/health/ready", nil)
		h.must(err)
		response, err := client.Do(request)
		if err == nil {
			h.must(response.Body.Close())
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if h.minio.exited() || time.Now().After(deadline) {
			h.t.Fatalf("MinIO readiness failed: %v", err)
		}
		h.pause(200 * time.Millisecond)
	}
	if fresh {
		h.native(filepath.Join(h.bin, "fixture"), "bucket-create", "--endpoint", "http://127.0.0.1:19000", "--bucket", "time-machine")
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
	h.native("/sbin/mount_smbfs", "-N", "//timemachine:synthetic-tm-control@127.0.0.1:1445/TimeMachine", h.share)
	h.native("/usr/bin/smbutil", "statshares", "-a")
	h.native("/sbin/mount")
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
	h.manifest(h.proof, filepath.Join(h.transfer, "reference/tree.jsonl"))
}
func (h *harness) randomFile(name string, size int64) {
	file, err := os.OpenFile(filepath.Join(h.proof, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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

func (h *harness) discover() map[string]any {
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
	h.save("proposed-exclusions.json", found, false)
	for _, path := range found {
		h.t.Logf("discover-directory %q", path)
	}
	h.t.Log("discover-image", os.Getenv("ImageOS"), os.Getenv("ImageVersion"))
	h.t.Log(h.native("/usr/bin/sw_vers"))
	h.t.Log(h.native("/sbin/mount"))
	h.createTree()
	h.must(os.Mkdir(filepath.Join(h.work, "objects"), 0o700))
	h.exclusions(true)
	for _, name := range []string{"Applications", "Library", "opt"} {
		for _, parent := range []string{"/", "/System/Volumes/Data"} {
			h.t.Log(h.native("/usr/bin/tmutil", "isexcluded", filepath.Join(parent, name)))
		}
	}
	h.checkExclusions()
	return map[string]any{"directories": found}
}
