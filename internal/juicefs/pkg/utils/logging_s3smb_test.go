// SPDX-License-Identifier: AGPL-3.0-only
package utils

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/logging"
	"github.com/sirupsen/logrus"
)

func TestNativeLoggingAndProgress(t *testing.T) {
	var out bytes.Buffer
	native := GetLogger("early-created-native")
	logging.Install(&out)
	if err := logging.Configure("json", "debug"); err != nil {
		t.Fatal(err)
	}
	logging.RegisterSecret("NATIVE_SECRET_MARKER")
	// Native setters and progress completion must not restore direct stderr output.
	SetLogLevel(logrus.ErrorLevel)
	InitLoggers(true)
	progress := NewProgress(false)
	if !progress.Quiet {
		t.Fatal("progress enabled")
	}
	b := progress.AddCountBar("PROGRESS_SECRET_MARKER", 1)
	b.Increment()
	progress.Done()
	native.Debug("NATIVE_SECRET_MARKER debug")
	native.Warn("permission warning NATIVE_SECRET_MARKER")
	native.Log("SDK-v1 NATIVE_SECRET_MARKER")
	if strings.Contains(out.String(), "NATIVE_SECRET_MARKER") || strings.Contains(out.String(), "PROGRESS_SECRET_MARKER") {
		t.Fatal("sensitive output")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("native output: %s", &out)
	}
	for _, line := range lines {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeFatalPanic(t *testing.T) {
	mode := os.Getenv("S3_SMB_NATIVE_LOG_CHILD")
	if mode != "" {
		logging.Install(os.Stderr)
		_ = logging.Configure("json", "error")
		logging.RegisterSecret("NATIVE_TERMINATION_SECRET")
		native := GetLogger("termination")
		if mode == "fatal" {
			native.Fatal("NATIVE_TERMINATION_SECRET")
		}
		native.Panic("NATIVE_TERMINATION_SECRET")
		return
	}
	for _, mode := range []string{"fatal", "panic"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestNativeFatalPanic$")
			cmd.Env = append(os.Environ(), "S3_SMB_NATIVE_LOG_CHILD="+mode)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if err == nil {
				t.Fatal("termination returned successfully")
			}
			if mode == "fatal" {
				if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 1 {
					t.Fatalf("fatal result: %v", err)
				}
			}
			if strings.Contains(stdout.String()+stderr.String(), "NATIVE_TERMINATION_SECRET") {
				t.Fatal("termination leaked secret")
			}
			// testing itself writes a failure banner to stdout for an unrecovered panic.
			if mode == "fatal" && stdout.Len() != 0 {
				t.Fatalf("unexpected stdout %q", stdout.String())
			}
			first := strings.SplitN(stderr.String(), "\n", 2)[0]
			var r map[string]any
			if err := json.Unmarshal([]byte(first), &r); err != nil {
				t.Fatalf("missing framed native diagnostic: %s", &stderr)
			}
		})
	}
}
