// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"

	smbproto "github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// The break tests run the Mac load of macLoad, or a file churn, under one
// kind of fault each. scripts/check.sh runs them in shards of their own.

// restartAndCheck stops the daemon cleanly, starts it again and checks
// every slot: a clean stop flushes, so every acknowledged byte must be there.
// Then, after a full FLUSH and a copy, it starts on a new data folder, which
// must restore the newest copy from S3 alone and show every byte again. The
// connections then go straight to the daemon's new address. Each check in also
// runs after the load's checks, through its first connection.
func restartAndCheck(l *macLoad, f *fixture, d *daemon, also ...func(*macConn)) *daemon {
	l.t.Helper()
	ctx := l.t.Context()
	l.closeAll(ctx)
	d.stop()
	d = f.start()
	l.connect(ctx)
	l.check(ctx, "a restart")
	for _, check := range also {
		check(l.conns[0])
	}
	if err := l.flush(ctx, 0, nil, true); err != nil {
		l.t.Fatal(err)
	}
	l.closeAll(ctx)
	f.copyDatabase(d)
	f.expireKilledLocks()
	f.freshLocal()
	newest := f.newestCopy()
	d = f.start()
	if restored := d.restoredCopy(); restored != newest {
		l.t.Fatalf("a new data folder restored copy %q, want the newest %q", restored, newest)
	}
	for i := range l.addrs {
		l.addrs[i], l.slow[i] = f.addr, false
	}
	l.connect(ctx)
	l.check(ctx, "a cold start from S3")
	for _, check := range also {
		check(l.conns[0])
	}
	return d
}

// TestBreakS3OutageUnderLoad cuts S3 for 75 seconds, or 5 minutes in the
// gate, while the Mac load fills the RAM budget. FLUSH, CLOSE, delete on
// close, lease breaks and the rest keep coming. Every request must get a
// reply or STATUS_PENDING within the patience, and STATUS_PENDING again
// while it waits longer than a minute. Nothing may be lost.
func TestBreakS3OutageUnderLoad(t *testing.T) {
	rng := chaosRand(t)
	rounds, outage := 2, 75*time.Second
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
			l.waitStuck(ctx, rng, proxy)
			// One client at a time, also when it is busy.
			if err := f.otherClientRefused(ctx, f.addr); err != nil {
				t.Error(err)
			}
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

// TestBreakFaultyS3UnderLoad runs the Mac load while S3 misbehaves, with an
// outage in each round. Even rounds draw a fault for every request on its
// own, so requests in flight end out of order, some fail while others
// succeed, and some are cut after S3 accepted them. Odd rounds switch
// errors, throttling, stalls and cuts for all requests every few seconds.
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
	for round := range rounds {
		l.loadRound(ctx, func() {
			stopFaults := func() {
				if err := proxy.SetMix(s3fault.Mix{}); err != nil {
					t.Error(err)
				}
			}
			if round%2 == 0 {
				if err := proxy.SetMix(s3fault.Mix{Fail: 0.3, Cut: 0.2, MaxDelay: 3 * time.Second}); err != nil {
					t.Fatal(err)
				}
			} else {
				stopFaults = startSchedule(t, rng, s3Faults(t, proxy), clearS3Faults(t, proxy)).stop
			}
			time.Sleep(between(rng, 0, length/2))
			outage := between(rng, 35*time.Second, 45*time.Second)
			start := proxy.FailS3For(outage)
			l.waitStuck(ctx, rng, proxy)
			time.Sleep(max(length/2, outage) - time.Since(start))
			stopFaults()
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
	l.fileFlushes = true
	l.connect(ctx)
	for range rounds {
		faults := startSchedule(t, rng, s3Faults(t, proxy), clearS3Faults(t, proxy))
		l.loadRound(ctx, func() {
			time.Sleep(between(rng, 2*time.Second, 8*time.Second))
			// Kill right after a FLUSH of one file, so that a FLUSH that
			// replied before its data was durable loses it.
			select {
			case <-l.flushed:
			default:
			}
			select {
			case <-l.flushed:
			case <-time.After(90 * time.Second):
				t.Error("coverage: no FLUSH of one file succeeded before the kill")
			}
			l.cutting(-1)
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
// every acknowledged WRITE must be there afterwards, and after the first
// round the daemon's file descriptors and threads must not keep growing.
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
			s3.FailS3For(between(rng, 35*time.Second, 45*time.Second))
			l.waitStuck(ctx, rng, s3)
			time.Sleep(between(rng, 4*time.Second, 8*time.Second))
			switch round % 3 {
			case 0:
				mode = "drop"
				l.cutting(0)
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
// as 32 workers can over both connections of the Mac load, at least 5 a
// second, while S3 misbehaves and goes away for a while. Flushes come in
// batches. Every file must be there with its bytes, and every deleted one
// gone, also after a restart and a cold start from S3.
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
	l := newMacLoad(t, f, rng, f.addr, f.addr)
	l.connect(ctx)
	c := &fileChurn{t: t, files: map[string][]byte{}, gone: map[string]bool{}}
	start := time.Now()
	l.loadRound(ctx, func() {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for w := range 32 {
			conn, source := l.conns[w%len(l.conns)], rand.New(rand.NewPCG(rng.Uint64(), uint64(w))) //nolint:gosec // Test faults need a replayable source.
			wg.Go(func() { c.work(ctx, conn, w, source, stop) })
		}
		wg.Go(func() {
			for !stopped(stop) && c.flush(ctx, l.conns[0]) == nil {
				time.Sleep(time.Second)
			}
		})
		faults := startSchedule(t, rng, s3Faults(t, proxy), clearS3Faults(t, proxy))
		time.Sleep(between(rng, 0, length/2))
		proxy.FailS3For(between(rng, 5*time.Second, length/3))
		time.Sleep(length - time.Since(start))
		close(stop)
		wg.Wait()
		faults.stop()
		proxy.RestoreS3()
	})
	if err := c.flush(ctx, l.conns[0]); err != nil {
		t.Fatal(err)
	}
	// A CREATE that waits for an upload of the file it opens holds up every
	// CREATE in the folder, which once brought the churn down to one a second.
	rate := float64(c.made.Load()+c.deleted.Load()) / time.Since(start).Seconds()
	t.Logf("churn: %d files made and %d deleted in %v, %.0f a second", c.made.Load(), c.deleted.Load(), time.Since(start), rate)
	if rate < 5 {
		t.Errorf("churn: %.1f files made or deleted a second, want at least 5", rate)
	}
	l.check(ctx, "S3 faults")
	c.check(ctx, l.conns[0])
	d = restartAndCheck(l, f, d, func(conn *macConn) { c.check(ctx, conn) })
	d.alive()
}

// fileChurn makes and deletes small files.
type fileChurn struct {
	t             *testing.T
	files         map[string][]byte
	gone          map[string]bool
	made, deleted atomic.Int64
	mu            sync.Mutex
}

// work creates a file with data in one compound, then now and then deletes
// one of its older files on close in another.
func (c *fileChurn) work(ctx context.Context, conn *macConn, w int, source *rand.Rand, stop <-chan struct{}) {
	for n := 0; !stopped(stop); n++ {
		name := fmt.Sprintf("churn-%d-%d", w, n)
		data := chaosData(source, source.IntN(64<<10))
		if _, err := conn.call(ctx, []wire.Message{
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
		replies, err := conn.call(ctx, []wire.Message{createMessage(c.t, smbtest.CreateOptions{Request: request}), relatedTo(closeMessage(c.t, related))}, smbproto.StatusObjectNameNotFound)
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
func (c *fileChurn) flush(ctx context.Context, conn *macConn) error {
	root := wire.CreateRequest{Name: "", DesiredAccess: 0x80, ShareAccess: 7, Disposition: 1}
	_, err := conn.call(ctx, []wire.Message{createMessage(c.t, smbtest.CreateOptions{Request: root}), relatedTo(flushMessage(c.t, related, true)), relatedTo(closeMessage(c.t, related))})
	if err != nil {
		c.t.Errorf("full FLUSH: %v", err)
	}
	return err
}

// check reads every file the churn made and did not delete, and checks that
// every deleted one is gone.
func (c *fileChurn) check(ctx context.Context, conn *macConn) {
	c.t.Helper()
	for name, want := range c.files {
		request := wire.CreateRequest{Name: name, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: 1}
		messages := []wire.Message{createMessage(c.t, smbtest.CreateOptions{Request: request}), relatedTo(readMessage(c.t, related, 0, 64<<10)), relatedTo(closeMessage(c.t, related))}
		replies, err := conn.call(ctx, messages, smbproto.StatusEndOfFile)
		if err != nil {
			c.t.Fatalf("%s: %v", name, err)
		}
		if got := readData(c.t, replies[1]); !bytes.Equal(got, want) {
			c.t.Fatalf("%s holds %d bytes, not its %d", name, len(got), len(want))
		}
	}
	for name := range c.gone {
		request := wire.CreateRequest{Name: name, DesiredAccess: 0x80, ShareAccess: 7, Disposition: 1}
		replies, err := conn.call(ctx, []wire.Message{createMessage(c.t, smbtest.CreateOptions{Request: request})}, smbproto.StatusObjectNameNotFound)
		if err != nil {
			c.t.Fatalf("%s: %v", name, err)
		}
		if replies[0].Header.Status != smbproto.StatusObjectNameNotFound {
			c.t.Fatalf("deleted %s is back", name)
		}
	}
}

// TestBreakLogoffDuringOutage logs off and disconnects trees on connections
// whose FLUSH waits on S3, while the Mac load runs on its own connections.
// LOGOFF and TREE_DISCONNECT close every open of their session or tree, so
// they wait for its requests. They must still get a reply or STATUS_PENDING
// within the patience.
func TestBreakLogoffDuringOutage(t *testing.T) {
	rng := chaosRand(t)
	rounds, outage := 2, 35*time.Second
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
			var leavers []*leaver
			for i, command := range []wire.Command{wire.Logoff, wire.TreeDisconnect} {
				leavers = append(leavers, prepareLeave(ctx, t, f, fmt.Sprintf("leaving-%d-%d", round, i), command))
			}
			start := proxy.FailS3For(outage)
			l.waitStuck(ctx, rng, proxy)
			var cleanups []*macRequest
			for _, leaving := range leavers {
				cleanups = append(cleanups, leaving.leave(ctx))
			}
			if time.Since(start) >= outage {
				t.Error("coverage: the cleanup came after the outage")
			}
			time.Sleep(outage - time.Since(start))
			for i, cleanup := range cleanups {
				if reply, err := leavers[i].conn.wait(cleanup); err != nil || reply.Header.Status != smbproto.StatusSuccess {
					t.Errorf("%v after the outage: %v, status %#x", leavers[i].command, err, reply.Header.Status)
				}
			}
		})
		l.check(ctx, "LOGOFF during an outage")
	}
	d = restartAndCheck(l, f, d)
	d.alive()
}

// leaver is a connection that is about to log off or disconnect its tree.
type leaver struct {
	t       *testing.T
	conn    *macConn
	ids     []wire.FileID // its own file, then every band
	command wire.Command
}

// prepareLeave connects and opens a file of its own and every band before
// the outage starts.
func prepareLeave(ctx context.Context, t *testing.T, f *fixture, name string, command wire.Command) *leaver {
	conn, err := f.macConnect(ctx, f.addr)
	if err != nil {
		t.Fatal(err)
	}
	l := &leaver{t: t, conn: conn, command: command}
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
		l.ids = append(l.ids, created.Reply.ID)
	}
	return l
}

// leave starts a WRITE and a FLUSH of its file, which wait on S3, and a READ
// of every band, which waits for the band's I/O while the load's stuck
// uploads hold it. Once the FLUSH has waited a second, it sends its command
// without waiting for any of them, and returns the command's request.
func (l *leaver) leave(ctx context.Context) *macRequest {
	if _, err := l.conn.start(ctx, []wire.Message{writeMessage(l.t, l.ids[0], 0, slotData(99, 0, 1))}); err != nil {
		l.t.Fatal(err)
	}
	flush, err := l.conn.start(ctx, []wire.Message{flushMessage(l.t, l.ids[0], false)})
	if err != nil {
		l.t.Fatal(err)
	}
	for _, id := range l.ids[1:] {
		if _, err = l.conn.start(ctx, []wire.Message{readMessage(l.t, id, slotSize, slotSize)}); err != nil {
			l.t.Fatal(err)
		}
	}
	time.Sleep(time.Second)
	if !l.conn.waiting(flush[0]) {
		l.t.Errorf("coverage: the FLUSH before %v did not wait on S3", l.command)
	}
	encode := wire.EncodeLogoffRequest
	if l.command == wire.TreeDisconnect {
		encode = wire.EncodeTreeDisconnectRequest
	}
	requests, err := l.conn.start(ctx, []wire.Message{emptyMessage(l.t, l.command, encode)})
	if err != nil {
		l.t.Fatal(err)
	}
	return requests[0]
}

// TestBreakDisk slows and fails the daemon's local disk under the Mac load:
// syncs that take seconds, syncs that fail with EIO and writes that fail with
// ENOSPC, on the SQLite database in the data folder. Slow syncs fail nothing,
// so the test kills the daemon right after a FLUSH that succeeded. The first
// failed write or sync must stop the daemon for good: after a failed fsync
// Linux can keep data only in memory, so running on is not safe. Either way
// the restart must keep its database, and every flushed byte must be there,
// also after a cold start from S3.
func TestBreakDisk(t *testing.T) {
	rng := chaosRand(t)
	rounds := 3
	if gate() {
		rounds = 9
	}
	f := newFixture(t)
	f.keepAlive, f.startupTimeout = true, 2*time.Minute
	control := filepath.Join(t.TempDir(), "control")
	setDisk := func(line string) {
		if err := os.WriteFile(control, []byte(line), 0o600); err != nil {
			t.Error(err)
		}
	}
	setDisk("none 0 0 0")
	f.env = []string{"LD_PRELOAD=" + buildDiskFault(t), "DISKFAULT_DIR=" + filepath.Join(f.root, "state"), "DISKFAULT_CONTROL=" + control}
	d := f.start()
	ctx := t.Context()
	l := newMacLoad(t, f, rng, f.addr, f.addr)
	l.failures, l.fileFlushes = true, true
	l.connect(ctx)
	faults := []string{"sync 1500 0 0", "sync 0 5 30", "write 0 28 30"}
	for round := range rounds {
		fault := faults[round%len(faults)]
		l.loadRound(ctx, func() {
			time.Sleep(between(rng, time.Second, 4*time.Second))
			if round%len(faults) > 0 {
				l.cutting(-1)
				setDisk(fault)
				err := d.waitExit(time.Minute)
				setDisk("none 0 0 0")
				if !bytes.Contains(d.output(), []byte("local disk")) {
					t.Errorf("%q: the daemon did not stop for the disk error: %v; logs %s", fault, err, d.path())
				}
				return
			}
			setDisk(fault)
			time.Sleep(between(rng, 10*time.Second, 20*time.Second))
			setDisk("none 0 0 0")
			d.alive()
			// Kill right after a FLUSH that succeeded once the disk is back.
			select {
			case <-l.flushed:
			default:
			}
			select {
			case <-l.flushed:
			case <-time.After(90 * time.Second):
				t.Error("coverage: no FLUSH of one file succeeded after the slow syncs")
			}
			l.cutting(-1)
			sigkill(t, d)
		})
		t.Logf("%q: %d requests failed", fault, l.failed.Swap(0))
		d = f.start()
		if !d.logged("keeping the local database") {
			t.Fatalf("after disk faults, the restart did not keep its database; logs %s", d.path())
		}
		l.connect(ctx)
		l.check(ctx, "kill")
	}
	f.env = nil
	d = restartAndCheck(l, f, d)
	d.alive()
}

// buildDiskFault compiles the disk fault library for LD_PRELOAD.
func buildDiskFault(t *testing.T) string {
	t.Helper()
	library := filepath.Join(t.TempDir(), "diskfault.so")
	build := exec.CommandContext(t.Context(), "cc", "-shared", "-fPIC", "-O2", "-o", library, "testdata/diskfault.c", "-ldl", "-lpthread") //nolint:gosec // The test builds its own library from its own source.
	output, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build the disk fault library: %v\n%s", err, output)
	}
	return library
}
