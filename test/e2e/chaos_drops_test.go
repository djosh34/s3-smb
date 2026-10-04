// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"
	smb "github.com/hirochachacha/go-smb2"
)

func TestChaosConnectionDrops(t *testing.T) {
	seed := chaos.Seed(t)
	rng := chaos.Rand(seed, "connection-drops")
	f := newChaosFixture(t, false)
	f.interval = "1h"
	f.cacheSize = "8 MB"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	s3Proxy, err := s3fault.New(ctx, f.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s3Proxy.Close(); err != nil {
			t.Error(err)
		}
	})
	f.endpoint = s3Proxy.URL()
	d := f.start()
	// Register after daemon cleanup so the log check includes shutdown output.
	t.Cleanup(func() {
		d.stop()
		for _, log := range d.logs {
			data, err := os.ReadFile(log.Name())
			if err != nil {
				t.Error(err)
				continue
			}
			if err := chaos.CheckDaemonLog(data); err != nil {
				t.Errorf("%s: %v", log.Name(), err)
			}
		}
	})
	network, err := netfault.New(ctx, f.addr)
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
	connect := func() (*smb.Share, func()) {
		t.Helper()
		share, closeShare := f.share()
		closeOnce := sync.OnceFunc(closeShare)
		t.Cleanup(closeOnce)
		return share.WithContext(ctx), closeOnce
	}
	check := func(share *smb.Share) {
		t.Helper()
		if err := ledger.CheckAcknowledged(chaosRead(share.ReadFile)); err != nil {
			t.Fatal(err)
		}
	}
	apply := func(schedule chaos.Schedule) {
		t.Helper()
		t.Logf("seed=%d schedule=%s", seed, schedule)
		if err := schedule.Run(ctx, network, s3Proxy); err != nil {
			t.Fatal(err)
		}
	}
	write := func(file *smb.File, name string, offset int64, data []byte) {
		t.Helper()
		ledger.Attempt(name, offset, data)
		n, err := file.WriteAt(data, offset)
		if err != nil || n != len(data) {
			t.Fatalf("write %s at %d: %d/%d bytes: %v", name, offset, n, len(data), err)
		}
		ledger.Write(name, offset, data)
	}
	payload := func() []byte {
		data := make([]byte, 64<<10)
		for i := range data {
			data[i] = byte(rng.Uint32())
		}
		return data
	}
	cut := func(direction netfault.Direction, operation func() error) {
		t.Helper()
		// Remove setup traffic so the observed bytes belong to this operation.
		for len(network.Events()) > 0 {
			<-network.Events()
		}
		cutAfter := int64(4096 + rng.IntN(24<<10))
		apply(chaos.Schedule{{Net: &netfault.Fault{CutAfter: cutAfter, CutDirection: direction}}})
		done := make(chan error, 1)
		go func() { done <- operation() }()
		deadline := time.NewTimer(20 * time.Second)
		defer deadline.Stop()
		droppedBefore := network.DroppedEvents()
		var forwarded int64
		observed := false
		for !observed {
			select {
			case event, ok := <-network.Events():
				if !ok {
					t.Fatal("network proxy stopped before the byte cut")
				}
				if event.Direction == direction {
					forwarded += event.Bytes
					observed = event.Cut
				}
			case <-deadline.C:
				t.Fatal("no byte-cut event within 20 seconds")
			case <-ctx.Done():
				t.Fatalf("waiting for byte cut: %v", ctx.Err())
			}
		}
		if network.DroppedEvents() != droppedBefore {
			t.Fatal("lost network events during the byte cut")
		}
		if forwarded != cutAfter {
			t.Fatalf("cut after %d forwarded bytes, want %d", forwarded, cutAfter)
		}
		select {
		case err := <-done:
			if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Fatalf("cut operation must fail visibly on the transport: %v", err)
			}
			t.Logf("seed=%d direction=%v cut_after=%d operation_error=%v", seed, direction, cutAfter, err)
		case <-deadline.C:
			t.Fatal("cut operation did not finish within 20 seconds")
		case <-ctx.Done():
			t.Fatalf("cut operation did not finish: %v", ctx.Err())
		}
		apply(chaos.Schedule{{Net: &netfault.Fault{}}})
	}
	share, closeShare := connect()
	file, err := share.Create("drops.bin")
	if err != nil {
		t.Fatal(err)
	}
	ledger.Truncate("drops.bin", 0)
	first, second := payload(), payload()
	write(file, "drops.bin", 0, first)
	write(file, "drops.bin", int64(len(first)), second)
	// No flush: rule 1 protects acknowledgments, not just durable writes.
	got := make([]byte, len(first))
	if n, err := file.ReadAt(got, 0); err != nil || n != len(got) || !bytes.Equal(got, first) {
		t.Fatalf("read before cut: %d bytes: %v", n, err)
	}
	cut(netfault.ServerToClient, func() error {
		_, err := file.ReadAt(got, int64(len(first)))
		return err
	})
	if err := file.Close(); err != nil {
		t.Logf("close handle on cut connection: %v", err)
	}
	closeShare()
	share, closeShare = connect()
	check(share)
	file, err = share.OpenFile("drops.bin", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	third := payload()
	offset := int64(len(first) + len(second))
	ledger.Attempt("drops.bin", offset, third)
	cut(netfault.ClientToServer, func() error {
		_, err := file.WriteAt(third, offset)
		return err
	})
	if err := file.Close(); err != nil {
		t.Logf("close handle on cut connection: %v", err)
	}
	closeShare()
	share, closeShare = connect()
	check(share)
	file, err = share.OpenFile("drops.bin", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	write(file, "drops.bin", offset, third)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	check(share)

	count := 32 + rng.IntN(16)
	outage := 2 * time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		count = 256 + rng.IntN(64)
		outage = 5 * time.Second
	}
	cutAt := count/2 + rng.IntN(count/4)
	file, err = share.Create("sequence.bin")
	if err != nil {
		t.Fatal(err)
	}
	ledger.Truncate("sequence.bin", 0)
	for i := range count {
		data := payload()
		offset := int64(i) * int64(len(data))
		if i == cutAt {
			ledger.Attempt("sequence.bin", offset, data)
			cut(netfault.ClientToServer, func() error {
				_, err := file.WriteAt(data, offset)
				return err
			})
			if err := file.Close(); err != nil {
				t.Logf("close handle on cut connection: %v", err)
			}
			closeShare()
			share, closeShare = connect()
			check(share)
			file, err = share.OpenFile("sequence.bin", os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
		}
		write(file, "sequence.bin", offset, data)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	check(share)
	apply(chaos.Schedule{{Net: &netfault.Fault{Drop: true}, Cut: true}})
	closeShare()
	// A fresh client must fail during the full outage, not just the old handle.
	unexpected, closeUnexpected, err := f.connect("backup", f.password)
	if err == nil {
		closeUnexpected()
		t.Fatalf("connected during full outage: %v", unexpected)
	}
	t.Logf("seed=%d outage=%s connection_error=%v", seed, outage, err)
	apply(chaos.Schedule{{At: outage, Net: &netfault.Fault{}}})
	share, _ = connect()
	check(share)
	file, err = share.OpenFile("sequence.bin", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(fmt.Sprintf("work continues after outage, seed %d\n", seed))
	write(file, "sequence.bin", int64(count)*(64<<10), data)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	check(share)
}
