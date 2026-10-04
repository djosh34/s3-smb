// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"
	smb "github.com/hirochachacha/go-smb2"
)

func TestChaosColdRecovery(t *testing.T) {
	seed := chaos.Seed(t)
	// Use the default daemon to check the scenario while the chaos fixture lands.
	f := newFixture(t, true)
	f.interval = "12s"
	proxy, err := s3fault.New(context.Background(), f.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	f.endpoint = proxy.URL()
	d := f.start()
	chaosColdLogCheck(t, d)
	network, err := netfault.New(context.Background(), f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := network.Close(); err != nil {
			t.Error(err)
		}
	})
	f.clientAddr = network.Address()

	ledger := chaos.NewLedger()
	payload := chaos.Rand(seed, "cold-data")
	large := make([]byte, 1_000_003)
	for i := range large {
		large[i] = byte(payload.Uint32())
	}
	chaosColdBaseline(t, f, ledger, large)
	f.protectedAfter(time.Now())
	chaosColdBeforeMark(t, f, seed, ledger, network, proxy, large)

	// No writes run while this snapshot is taken. Its receipt is published only
	// after the metadata upload has been read back and verified.
	f.protectedAfter(time.Now())
	receipt := f.receipt()
	if receipt.Key == "" || receipt.UUID == "" || len(receipt.SHA256) != 64 {
		t.Fatalf("incomplete verified metadata receipt: %+v", receipt)
	}
	mark := ledger.Mark()
	t.Logf("cold recovery mark: seed=%d snapshot=%s key=%s", seed, receipt.Snapshot.Format(time.RFC3339Nano), receipt.Key)
	// Pin the recovery point even on slow CI runners. Check the remote objects
	// after the kill too, since an uploaded snapshot might lack a local receipt.
	proxy.SetMetadataFailure(true)
	chaosColdAfterMark(t, f, ledger, large)
	sigkill(t, d)
	if got := f.receipt(); got.Key != receipt.Key {
		t.Fatalf("metadata receipt moved after the mark: %s to %s", receipt.Key, got.Key)
	}
	chaosColdLatest(t, f, receipt.Key)
	proxy.SetMetadataFailure(false)

	// Delete config, metadata, keys and cache, not just the database. Recovery
	// reads the same bucket with new local state and no network fault proxy.
	f.clientAddr = ""
	f.freshLocal()
	d = f.start()
	chaosColdLogCheck(t, d)
	chaosColdRecovered(t, f, ledger, mark)
	d.stop()
}

func chaosColdBaseline(t *testing.T, f *fixture, ledger *chaos.Ledger, large []byte) {
	t.Helper()
	share, closeShare := f.share()
	defer closeShare()
	chaosColdWrite(t, share, ledger, "earlier-empty", nil)
	chaosColdWrite(t, share, ledger, "earlier-文件.txt", []byte("an earlier backup stays byte-correct\n"))
	chaosColdWrite(t, share, ledger, "earlier-large.bin", large)
}

func chaosColdBeforeMark(t *testing.T, f *fixture, seed uint64, ledger *chaos.Ledger, network *netfault.Proxy, proxy *s3fault.Proxy, large []byte) {
	t.Helper()
	share, closeShare := f.share()
	defer closeShare()
	chaosColdFaultRun(t, seed, share, ledger, network, proxy, large)
	chaosColdWrite(t, share, ledger, "removed-before-mark", []byte("not in the protected dataset"))
	if err := share.Remove("removed-before-mark"); err != nil {
		t.Fatal(err)
	}
	ledger.Remove("removed-before-mark")
	if err := ledger.CheckAcknowledged(share.ReadFile); err != nil {
		t.Fatal(err)
	}
}

func chaosColdAfterMark(t *testing.T, f *fixture, ledger *chaos.Ledger, large []byte) {
	t.Helper()
	share, closeShare := f.share()
	defer closeShare()
	chaosColdWrite(t, share, ledger, "chaos.bin", large[:513])
	if err := share.Truncate("earlier-large.bin", 17); err != nil {
		t.Fatal(err)
	}
	ledger.Truncate("earlier-large.bin", 17)
	if err := share.Remove("earlier-文件.txt"); err != nil {
		t.Fatal(err)
	}
	ledger.Remove("earlier-文件.txt")
	chaosColdWrite(t, share, ledger, "after-mark", []byte("lost with the original machine"))
	if err := ledger.CheckAcknowledged(share.ReadFile); err != nil {
		t.Fatal(err)
	}
}

func chaosColdRecovered(t *testing.T, f *fixture, ledger *chaos.Ledger, mark chaos.Mark) {
	t.Helper()
	share, closeShare := f.share()
	defer closeShare()
	if err := ledger.CheckMark(mark, share.ReadFile); err != nil {
		t.Fatal(err)
	}
	entries, err := share.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	want := []string{"chaos.bin", "earlier-empty", "earlier-large.bin", "earlier-文件.txt"}
	if !slices.Equal(names, want) {
		t.Fatalf("recovered names: got %q, want %q", names, want)
	}
	// Start a ledger from the recovered state, not the lost post-mark writes.
	recovered := chaos.NewLedger()
	for _, name := range want {
		data, err := share.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		recovered.Truncate(name, 0)
		recovered.Write(name, 0, data)
	}
	chaosColdWrite(t, share, recovered, "after-recovery", []byte("the recovered daemon accepts writes\n"))
	if err := recovered.CheckAcknowledged(share.ReadFile); err != nil {
		t.Fatal(err)
	}
}

func chaosColdFaultRun(t *testing.T, seed uint64, share *smb.Share, ledger *chaos.Ledger, network *netfault.Proxy, proxy *s3fault.Proxy, data []byte) {
	t.Helper()
	random := chaos.Rand(seed, "cold-faults")
	stall := 100*time.Millisecond + time.Duration(random.IntN(100))*time.Millisecond
	outage := time.Second + time.Duration(random.IntN(250))*time.Millisecond
	delay := time.Duration(1+random.IntN(3)) * time.Millisecond
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		stall *= 10
		outage *= 5
		delay *= 2
	}
	// Install the first faults before issuing CREATE so both proxies affect work.
	setup := chaos.Schedule{{Net: &netfault.Fault{Stall: true}, S3Outage: outage}}
	schedule := chaos.Schedule{
		{At: stall, Net: &netfault.Fault{Delay: delay}},
		{At: outage + time.Second, Net: &netfault.Fault{}, S3: &s3fault.Fault{}},
	}
	t.Logf("cold fault setup:\n%s\ncold fault schedule:\n%s", setup.String(), schedule.String())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := setup.Run(ctx, network, proxy); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- schedule.Run(ctx, network, proxy) }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	}()
	chaosColdWrite(t, share, ledger, "chaos.bin", data)
	select {
	case event := <-proxy.OutageSeen():
		t.Logf("S3 fault reached %s %s: %d", event.Method, event.Path, event.Status)
	default:
		t.Fatal("chaos write never reached the S3 outage")
	}
	err := <-done
	joined = true
	if err != nil {
		t.Fatal(err)
	}
}

func chaosColdWrite(t *testing.T, share *smb.Share, ledger *chaos.Ledger, name string, data []byte) {
	t.Helper()
	file, err := share.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	ledger.Truncate(name, 0)
	ledger.Attempt(name, 0, data)
	n, err := file.Write(data)
	if err != nil {
		t.Fatalf("write %s: %d/%d: %v", name, n, len(data), err)
	}
	if n != len(data) {
		t.Fatalf("write %s: %d/%d: %v", name, n, len(data), io.ErrShortWrite)
	}
	ledger.Write(name, 0, data)
	if err := file.Sync(); err != nil {
		t.Fatalf("flush %s: %v", name, err)
	}
	ledger.Flush(name)
}

func chaosColdLatest(t *testing.T, f *fixture, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pages := s3.NewListObjectsV2Paginator(f.store, &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket), Prefix: aws.String("meta/snapshot-")})
	var latest string
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if strings.HasSuffix(key, ".db.gz") && key > latest {
				latest = key
			}
		}
	}
	if latest != want {
		t.Fatalf("latest remote metadata backup changed after mark: got %s, want %s", latest, want)
	}
}

func chaosColdLogCheck(t *testing.T, d *daemon) {
	t.Helper()
	// Run after the fixture's stop cleanup, including when an assertion fails.
	// sigkill and an explicit stop have already closed the files on success.
	paths := make([]string, 0, len(d.logs))
	for _, log := range d.logs {
		paths = append(paths, log.Name())
	}
	t.Cleanup(func() {
		d.stop()
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Error(err)
				continue
			}
			if err := chaos.CheckDaemonLog(data); err != nil {
				t.Errorf("%s: %v", path, err)
			}
		}
	})
}
