// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"

	smbproto "github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// The break tests run the Mac load of macLoad, or a file churn, under one
// kind of fault each. scripts/check.sh runs them in shards of their own.

// restartAndCheck stops the daemon cleanly, starts it again and checks every
// slot. A clean stop flushes, so every acknowledged byte must be there.
func restartAndCheck(l *macLoad, f *fixture, d *daemon) *daemon {
	l.t.Helper()
	ctx := l.t.Context()
	l.closeAll(ctx)
	d.stop()
	d = f.start()
	l.connect(ctx)
	l.check(ctx, "restart")
	return d
}

// TestBreakS3OutageUnderLoad cuts S3 for 25 seconds, or 5 minutes in the
// gate, while the Mac load fills the RAM budget. FLUSH, CLOSE, delete on
// close, lease breaks and the rest keep coming. Every request must get a
// reply or STATUS_PENDING within the patience, and nothing may be lost.
func TestBreakS3OutageUnderLoad(t *testing.T) {
	rng := chaosRand(t)
	rounds, outage := 2, 25*time.Second
	if gate() {
		rounds, outage = 4, 5*time.Minute
	}
	f := newFixture(t)
	proxy := f.newFaultProxy()
	t.Cleanup(proxy.RestoreS3)
	d := f.start()
	ctx := t.Context()
	l := newMacLoad(t, f, rng, f.addr, f.addr)
	l.connect(ctx)
	for range rounds {
		puts := proxy.ChunkPuts()
		l.loadRound(ctx, func() {
			time.Sleep(between(rng, time.Second, 5*time.Second))
			t.Log("outage starts after", proxy.ChunkPuts()-puts, "chunk PUTs")
			start := proxy.FailS3For(outage)
			l.waitStuck(ctx, rng)
			time.Sleep(outage - time.Since(start))
		})
		l.check(ctx, "an S3 outage")
		if err := l.flush(ctx, 0, nil, true); err != nil {
			t.Fatal(err)
		}
	}
	d = restartAndCheck(l, f, d)
	d.alive()
}

// TestBreakFaultyS3UnderLoad runs the Mac load while S3 fails, throttles,
// stalls and cuts requests, with a short outage in each round.
func TestBreakFaultyS3UnderLoad(t *testing.T) {
	rng := chaosRand(t)
	rounds, length := 2, 30*time.Second
	if gate() {
		rounds, length = 6, 3*time.Minute
	}
	f := newFixture(t)
	f.keepAlive, f.startupTimeout = true, 2*time.Minute
	proxy := f.newFaultProxy()
	t.Cleanup(proxy.RestoreS3)
	d := f.start()
	ctx := t.Context()
	l := newMacLoad(t, f, rng, f.addr, f.addr)
	l.connect(ctx)
	for range rounds {
		l.loadRound(ctx, func() {
			faults := startSchedule(t, rng, s3Faults(t, proxy), clearS3Faults(t, proxy))
			time.Sleep(between(rng, 0, length/2))
			start := proxy.FailS3For(between(rng, 15*time.Second, 25*time.Second))
			l.waitStuck(ctx, rng)
			time.Sleep(length/2 - time.Since(start))
			faults.stop()
		})
		l.check(ctx, "S3 faults")
		if err := l.flush(ctx, 0, nil, true); err != nil {
			t.Fatal(err)
		}
	}
	d = restartAndCheck(l, f, d)
	d.alive()
}

// TestBreakKillUnderLoad kills the daemon at a random moment of the Mac load,
// while S3 misbehaves, and starts it again on the same data folder. Every
// slot must hold its last flushed version or one written after it.
func TestBreakKillUnderLoad(t *testing.T) {
	rng := chaosRand(t)
	rounds := 3
	if gate() {
		rounds = 10
	}
	f := newFixture(t)
	f.keepAlive, f.startupTimeout = true, 2*time.Minute
	proxy := f.newFaultProxy()
	t.Cleanup(proxy.RestoreS3)
	d := f.start()
	ctx := t.Context()
	l := newMacLoad(t, f, rng, f.addr, f.addr)
	l.connect(ctx)
	for range rounds {
		faults := startSchedule(t, rng, s3Faults(t, proxy), clearS3Faults(t, proxy))
		l.loadRound(ctx, func() {
			time.Sleep(between(rng, 2*time.Second, 12*time.Second))
			sigkill(t, d)
		})
		faults.stop()
		d = f.start()
		l.connect(ctx)
		l.check(ctx, "kill")
	}
	d = restartAndCheck(l, f, d)
	d.alive()
}

// TestBreakNetworkUnderLoad poisons one of the Mac's two connections while
// the load runs and S3 is down: each round cuts it, holds it silent for up
// to 30 seconds with no FIN or RST, or lets bytes trickle through it. The
// other connection must keep its progress. The server keeps running, so
// every acknowledged WRITE must be there afterwards, and the daemon must not
// keep more file descriptors or threads than it started with.
func TestBreakNetworkUnderLoad(t *testing.T) {
	rng := chaosRand(t)
	rounds := 3
	if gate() {
		rounds = 12
	}
	f := newFixture(t)
	s3 := f.newFaultProxy()
	t.Cleanup(s3.RestoreS3)
	d := f.start()
	network := f.networkProxy()
	ctx := t.Context()
	l := newMacLoad(t, f, rng, network.Address(), f.addr)
	l.slow[0] = true
	l.connect(ctx)
	// Pools and threads grow in the first round. Later rounds must not keep
	// growing.
	var baseline resources
	for round := range rounds {
		mode := "quiet"
		l.loadRound(ctx, func() {
			time.Sleep(between(rng, time.Second, 6*time.Second))
			s3.FailS3For(between(rng, 20*time.Second, 30*time.Second))
			l.waitStuck(ctx, rng)
			time.Sleep(between(rng, 4*time.Second, 8*time.Second))
			switch round % 3 {
			case 0:
				mode = "drop"
				if err := network.Drop(); err != nil {
					t.Error(err)
				}
				time.Sleep(between(rng, 0, 10*time.Second))
				network.Restore()
			case 1:
				stall := between(rng, 5*time.Second, 30*time.Second)
				network.Stall(stall)
				time.Sleep(stall)
			case 2:
				network.SetShape(netfault.Shape{Delay: 50 * time.Millisecond, Rate: 64 << 10})
				time.Sleep(between(rng, 10*time.Second, 20*time.Second))
				network.SetShape(netfault.Shape{})
			}
		})
		s3.RestoreS3()
		if mode == "drop" {
			l.closeAll(ctx)
			l.connect(ctx)
		}
		l.check(ctx, mode)
		now := daemonResources(t, d)
		t.Logf("round %d: the daemon holds %+v", round, now)
		if round == 0 {
			baseline = now
		} else if now.fds > baseline.fds+8 || now.threads > baseline.threads+8 {
			t.Errorf("the daemon holds %+v after round %d, from %+v after the first", now, round, baseline)
		}
	}
	d = restartAndCheck(l, f, d)
	d.alive()
}

// resources is what a process holds.
type resources struct{ fds, threads int }

// daemonResources counts the daemon's open file descriptors and threads.
func daemonResources(t *testing.T, d *daemon) resources {
	t.Helper()
	pid := d.cmd.Process.Pid
	fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		t.Fatal(err)
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	r := resources{fds: len(fds)}
	for line := range strings.Lines(string(status)) {
		if value, ok := strings.CutPrefix(line, "Threads:"); ok {
			if r.threads, err = strconv.Atoi(strings.TrimSpace(value)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return r
}

// TestBreakFileChurn creates, writes, closes and deletes as many small files
// as it can on one connection, offering about a thousand a second, while S3
// misbehaves and goes away for a while. Flushes come in batches. Every file
// must be there with its bytes, and every deleted one gone, also after a
// restart.
func TestBreakFileChurn(t *testing.T) {
	rng := chaosRand(t)
	length := 30 * time.Second
	if gate() {
		length = 10 * time.Minute
	}
	f := newFixture(t)
	f.keepAlive, f.startupTimeout = true, 2*time.Minute
	proxy := f.newFaultProxy()
	t.Cleanup(proxy.RestoreS3)
	d := f.start()
	ctx := t.Context()
	conn, err := f.macConnect(ctx, f.addr)
	if err != nil {
		t.Fatal(err)
	}
	c := &fileChurn{t: t, conn: conn, files: map[string][]byte{}, gone: map[string]bool{}}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 32 {
		source := rand.New(rand.NewPCG(rng.Uint64(), uint64(w))) //nolint:gosec // Test faults need a replayable source.
		wg.Go(func() { c.work(ctx, w, source, stop) })
	}
	wg.Go(func() {
		for !stopped(stop) && c.flush(ctx) == nil {
			time.Sleep(time.Second)
		}
	})
	faults := startSchedule(t, rng, s3Faults(t, proxy), clearS3Faults(t, proxy))
	start := time.Now()
	time.Sleep(between(rng, 0, length/2))
	proxy.FailS3For(between(rng, 5*time.Second, length/3))
	time.Sleep(length - time.Since(start))
	close(stop)
	wg.Wait()
	faults.stop()
	proxy.RestoreS3()
	if err = c.flush(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("churn: %d files made and %d deleted in %v, %.0f a second", c.made.Load(), c.deleted.Load(), time.Since(start), float64(c.made.Load()+c.deleted.Load())/time.Since(start).Seconds())
	c.check(ctx)
	if err = conn.close(ctx); err != nil {
		t.Fatal(err)
	}
	d.stop()
	d = f.start()
	if c.conn, err = f.macConnect(ctx, f.addr); err != nil {
		t.Fatal(err)
	}
	c.check(ctx)
	d.alive()
}

// fileChurn makes and deletes small files on one connection.
type fileChurn struct {
	t             *testing.T
	conn          *macConn
	files         map[string][]byte
	gone          map[string]bool
	made, deleted atomic.Int64
	mu            sync.Mutex
}

// work creates a file with data in one compound, then now and then deletes
// one of its older files on close in another.
func (c *fileChurn) work(ctx context.Context, w int, source *rand.Rand, stop <-chan struct{}) {
	for n := 0; !stopped(stop); n++ {
		name := fmt.Sprintf("churn-%d-%d", w, n)
		data := chaosData(source, source.IntN(64<<10))
		if _, err := c.conn.call(ctx, []wire.Message{
			createMessage(c.t, smbtest.CreateOptions{Request: wire.CreateRequest{Name: name, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: 2}}),
			relatedTo(writeMessage(c.t, related, 0, data)), relatedTo(closeMessage(c.t, related)),
		}); err != nil {
			c.t.Errorf("create %s: %v", name, err)
			return
		}
		c.made.Add(1)
		c.mu.Lock()
		c.files[name] = data
		c.mu.Unlock()
		if n == 0 || source.IntN(2) != 0 {
			continue
		}
		old := fmt.Sprintf("churn-%d-%d", w, source.IntN(n))
		request := wire.CreateRequest{Name: old, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: 1, Options: 0x1000}
		replies, err := c.conn.call(ctx, []wire.Message{createMessage(c.t, smbtest.CreateOptions{Request: request}), relatedTo(closeMessage(c.t, related))}, smbproto.StatusObjectNameNotFound)
		if err != nil {
			c.t.Errorf("delete %s: %v", old, err)
			return
		}
		if replies[0].Header.Status == smbproto.StatusSuccess {
			c.deleted.Add(1)
			c.mu.Lock()
			delete(c.files, old)
			c.gone[old] = true
			c.mu.Unlock()
		}
	}
}

// flush flushes every file, through the share's root.
func (c *fileChurn) flush(ctx context.Context) error {
	root := wire.CreateRequest{Name: "", DesiredAccess: 0x80, ShareAccess: 7, Disposition: 1}
	_, err := c.conn.call(ctx, []wire.Message{createMessage(c.t, smbtest.CreateOptions{Request: root}), relatedTo(flushMessage(c.t, related, true)), relatedTo(closeMessage(c.t, related))})
	if err != nil {
		c.t.Errorf("full FLUSH: %v", err)
	}
	return err
}

// check reads every file the churn made and did not delete, and checks that
// every deleted one is gone.
func (c *fileChurn) check(ctx context.Context) {
	c.t.Helper()
	for name, want := range c.files {
		request := wire.CreateRequest{Name: name, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: 1}
		messages := []wire.Message{createMessage(c.t, smbtest.CreateOptions{Request: request}), relatedTo(readMessage(c.t, related, 0, 64<<10)), relatedTo(closeMessage(c.t, related))}
		replies, err := c.conn.call(ctx, messages, smbproto.StatusEndOfFile)
		if err != nil {
			c.t.Fatalf("%s: %v", name, err)
		}
		if got := readData(c.t, replies[1]); !bytes.Equal(got, want) {
			c.t.Fatalf("%s holds %d bytes, not its %d", name, len(got), len(want))
		}
	}
	for name := range c.gone {
		request := wire.CreateRequest{Name: name, DesiredAccess: 0x80, ShareAccess: 7, Disposition: 1}
		replies, err := c.conn.call(ctx, []wire.Message{createMessage(c.t, smbtest.CreateOptions{Request: request})}, smbproto.StatusObjectNameNotFound)
		if err != nil {
			c.t.Fatalf("%s: %v", name, err)
		}
		if replies[0].Header.Status != smbproto.StatusObjectNameNotFound {
			c.t.Fatalf("deleted %s is back", name)
		}
	}
}

// TestBreakLogoffDuringOutage logs off and disconnects trees on connections
// whose FLUSH waits on S3, while the Mac load runs on its own connection.
// LOGOFF and TREE_DISCONNECT close every open of their session or tree, so
// they wait for its requests. They must still get a reply or STATUS_PENDING
// within the patience.
func TestBreakLogoffDuringOutage(t *testing.T) {
	rng := chaosRand(t)
	rounds, outage := 2, 25*time.Second
	if gate() {
		rounds, outage = 4, 3*time.Minute
	}
	f := newFixture(t)
	proxy := f.newFaultProxy()
	t.Cleanup(proxy.RestoreS3)
	d := f.start()
	ctx := t.Context()
	l := newMacLoad(t, f, rng, f.addr, f.addr)
	l.connect(ctx)
	for round := range rounds {
		l.loadRound(ctx, func() {
			time.Sleep(between(rng, 2*time.Second, 5*time.Second))
			start := proxy.FailS3For(outage)
			l.waitStuck(ctx, rng)
			for i, command := range []wire.Command{wire.Logoff, wire.TreeDisconnect} {
				leaveDuringOutage(ctx, t, f, fmt.Sprintf("leaving-%d-%d", round, i), command)
			}
			time.Sleep(outage - time.Since(start))
		})
		l.check(ctx, "LOGOFF during an outage")
	}
	d = restartAndCheck(l, f, d)
	d.alive()
}

// leaveDuringOutage writes a file on a new connection and starts a FLUSH of
// it that waits on S3, and a READ of a band that waits for the band's I/O,
// which the load's stuck uploads hold. Then it sends command without waiting
// for either.
func leaveDuringOutage(ctx context.Context, t *testing.T, f *fixture, name string, command wire.Command) {
	conn, err := f.macConnect(ctx, f.addr)
	if err != nil {
		t.Fatal(err)
	}
	var ids []wire.FileID
	files := []string{name}
	for b := range loadBands {
		files = append(files, bandName(b))
	}
	for _, file := range files {
		request := wire.CreateRequest{Name: file, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf}
		replies, callErr := conn.call(ctx, []wire.Message{createMessage(t, smbtest.CreateOptions{Request: request})})
		if callErr != nil {
			t.Fatal(callErr)
		}
		created, decodeErr := smbtest.DecodeCreateReply(replies[0])
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		ids = append(ids, created.Reply.ID)
	}
	if _, err = conn.call(ctx, []wire.Message{writeMessage(t, ids[0], 0, slotData(99, 0, 1))}); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.start(ctx, []wire.Message{flushMessage(t, ids[0], false)}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[1:] {
		if _, err = conn.start(ctx, []wire.Message{readMessage(t, id, slotSize, slotSize)}); err != nil {
			t.Fatal(err)
		}
	}
	encode := wire.EncodeLogoffRequest
	if command == wire.TreeDisconnect {
		encode = wire.EncodeTreeDisconnectRequest
	}
	if _, err = conn.start(ctx, []wire.Message{emptyMessage(t, command, encode)}); err != nil {
		t.Fatal(err)
	}
}
