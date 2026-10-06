// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"testing"
	"time"
)

// TestBucketLockAtStartup starts a second server, with its own data folder, on
// the bucket of a running one. The second waits and does not serve while the
// first runs. Once the first stops cleanly, the second takes over without the
// 10 minute wait and restores the newest copy.
func TestBucketLockAtStartup(t *testing.T) {
	f := newFixture(t)
	d := f.start()
	share, disconnect := f.share()
	files := map[string][]byte{"first.txt": []byte("written by the first server\n")}
	writeFiles(t, share, files)
	disconnect()
	f.copyDatabase(d)
	d = f.start()
	second := f.another()
	waiting := second.launch()
	waiting.waitLogged("another server holds the bucket; waiting until its lock goes stale", 30*time.Second)
	if waiting.logged("SMB serving") {
		t.Fatal("the second server serves while the first runs")
	}
	share, disconnect = f.share()
	verifyFiles(t, share, files)
	disconnect()
	d.stop()
	newest := f.newestCopy()
	// The waiting server tries again after 15 to 45 seconds.
	waiting.waitLogged("SMB serving", 2*time.Minute)
	if restored := waiting.restoredCopy(); restored != newest {
		t.Fatalf("the second server restored copy %q, want the newest %q", restored, newest)
	}
	share, disconnect = second.share()
	verifyFiles(t, share, files)
	disconnect()
	waiting.stop()
}

// TestStartupBadCredentials starts with a wrong S3 secret key. The daemon
// fails during startup, before it serves, and writes nothing to the bucket.
func TestStartupBadCredentials(t *testing.T) {
	f := newFixture(t)
	f.signingKey = wrongSigningKey
	f.failStart = true
	d := f.start()
	if !bytes.Contains(d.read("stderr.log"), []byte("SignatureDoesNotMatch")) {
		t.Fatalf("startup did not report the rejected credentials; logs %s", d.path())
	}
	if keys := f.keys(""); len(keys) != 0 {
		t.Fatalf("a failed startup wrote %v", keys)
	}
}
