package config

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Regression for clean spec review's TestReviewHelperOrphanBound: a successful
// wrapper exits while its child retains the output pipes, reaching WaitDelay
// rather than CommandContext.Cancel. No implicit shell is added by the resolver;
// this test explicitly chooses one to reproduce that process-tree shape.
func TestHelperDescendantTerminatedOnWaitDelay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	_, err := runHelper(context.Background(), ".", []string{"/bin/sh", "-c", "sleep 30 & echo $! > \"$1\"; printf credential", "probe", path})
	if err == nil {
		t.Fatal("helper retaining pipes unexpectedly succeeded")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("inherited helper pipes exceeded wait bound")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		if runtime.GOOS == "linux" {
			// An orphan may await PID1 reaping after SIGKILL. A zombie has terminated;
			// kill(pid,0) alone would falsely report it as still executing.
			stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			if os.IsNotExist(err) {
				return
			}
			if err == nil {
				fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
				if len(fields) > 0 && fields[0] == "Z" {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("helper descendant is still running after resolver returns")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
