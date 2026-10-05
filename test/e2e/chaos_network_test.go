// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
)

// piece is the size of each WRITE and READ the chaos tests send.
const piece = 64 << 10

// TestChaosSlowNetwork runs a backup over a slow, unsteady link that stalls
// for up to 20 seconds, or 5 seconds outside the gate. Every request succeeds
// with the right bytes.
func TestChaosSlowNetwork(t *testing.T) {
	rng := chaosRand(t)
	count, maxStall := 4, 5*time.Second
	if gate() {
		count, maxStall = 16, 20*time.Second
	}
	f := chaosFixture(t)
	d := f.start()
	proxy := f.networkProxy()
	// Log in first: a long stall would outlast the login timeout.
	share, disconnect := f.chaosShare(proxy.Address())
	faults := startSchedule(t, rng, networkFaults(proxy, maxStall), clearNetworkFaults(proxy))
	files := chaosFiles(rng, "slow-network", count, 2<<20)
	writeFiles(t, share, files)
	verifyFiles(t, share, files)
	disconnect()
	faults.stop()
	d.alive()
}

// TestChaosConnectionCuts cuts the connection at a random point of a write and
// of a read. The server keeps every acknowledged write, lets the Mac back in
// and never returns wrong bytes.
func TestChaosConnectionCuts(t *testing.T) {
	rng := chaosRand(t)
	rounds := 3
	if gate() {
		rounds = 12
	}
	f := chaosFixture(t)
	d := f.start()
	proxy := f.networkProxy()
	for round := range rounds {
		name := fmt.Sprintf("cut-%d.bin", round)
		data := chaosData(rng, 4<<20)
		cutWrite(t, f, proxy, rng, name, data)
		cutRead(t, f, proxy, rng, name, data)
	}
	d.alive()
}

// cutWrite writes data in pieces and cuts the connection after a random
// number of them. Each piece the server acknowledged must be in the file. It
// then writes the whole file again.
func cutWrite(t *testing.T, f *fixture, proxy *netfault.Proxy, rng *rand.Rand, name string, data []byte) {
	t.Helper()
	// The cut ends this connection, so it is never logged off.
	share, _ := f.chaosShare(proxy.Address())
	file, err := share.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	cutAt := rng.IntN(len(data) / piece)
	reached, acknowledged := make(chan struct{}), make(chan int, 1)
	reach := sync.OnceFunc(func() { close(reached) })
	go func() {
		// A WRITE that fails before the cut point must not leave the test
		// waiting.
		defer reach()
		written := 0
		for ; written < len(data); written += piece {
			if written == cutAt*piece {
				reach()
			}
			if _, writeErr := file.WriteAt(data[written:written+piece], int64(written)); writeErr != nil {
				break
			}
		}
		acknowledged <- written
	}()
	<-reached
	cut(t, proxy, rng)
	written := <-acknowledged
	if written < cutAt*piece {
		t.Fatalf("WRITE failed at %d before the cut", written)
	}
	share, disconnect := f.chaosShare(proxy.Address())
	defer disconnect()
	got, err := share.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < written || len(got) > len(data) || !bytes.Equal(got, data[:len(got)]) {
		t.Fatalf("after a cut at %d of %d acknowledged bytes the file holds %d bytes, not a prefix of the written data", cutAt*piece, written, len(got))
	}
	writeFile(t, share, name, data)
}

// cutRead reads the file in pieces and cuts the connection after a random
// number of them. Every piece read before the cut and the whole file read
// after it must be right.
func cutRead(t *testing.T, f *fixture, proxy *netfault.Proxy, rng *rand.Rand, name string, data []byte) {
	t.Helper()
	share, _ := f.chaosShare(proxy.Address())
	file, err := share.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	cutAt := rng.IntN(len(data) / piece)
	reached, ended := make(chan struct{}), make(chan error, 1)
	reach := sync.OnceFunc(func() { close(reached) })
	go func() {
		defer reach()
		got := make([]byte, piece)
		for offset := 0; offset < len(data); offset += piece {
			if offset == cutAt*piece {
				reach()
			}
			n, err := file.ReadAt(got, int64(offset))
			if !bytes.Equal(got[:n], data[offset:offset+n]) {
				ended <- fmt.Errorf("READ at %d returned wrong bytes", offset)
				return
			}
			if err != nil && !errors.Is(err, io.EOF) {
				// An error after the cut point is the cut.
				if offset < cutAt*piece {
					err = fmt.Errorf("READ at %d failed before the cut: %w", offset, err)
				} else {
					err = nil
				}
				ended <- err
				return
			}
		}
		ended <- nil
	}()
	<-reached
	cut(t, proxy, rng)
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	share, disconnect := f.chaosShare(proxy.Address())
	defer disconnect()
	verifyFiles(t, share, map[string][]byte{name: data})
}

// cut drops all connections through proxy and lets new ones in after up to a
// second.
func cut(t *testing.T, proxy *netfault.Proxy, rng *rand.Rand) {
	t.Helper()
	if err := proxy.Drop(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(between(rng, 0, time.Second))
	proxy.Restore()
}
