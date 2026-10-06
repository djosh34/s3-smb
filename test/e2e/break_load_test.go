// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	smbproto "github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// slotSize is the size of every band write: Time Machine writes bands in
// 1 MiB pieces.
const slotSize = 1 << 20

// slotData is version v of a slot: a header that names it, then bytes only it
// has. Version 0 is the zeros of a slot never written.
func slotData(file, slot int, v uint32) []byte {
	data := make([]byte, slotSize)
	binary.LittleEndian.PutUint64(data, 0x6b616572622d3036)
	binary.LittleEndian.PutUint32(data[8:], uint32(file))  //nolint:gosec // Small test indexes.
	binary.LittleEndian.PutUint32(data[12:], uint32(slot)) //nolint:gosec // Small test indexes.
	binary.LittleEndian.PutUint32(data[16:], v)
	rng := rand.New(rand.NewPCG(uint64(file)<<32|uint64(slot), uint64(v))) //nolint:gosec // Data, not secrets.
	for i := 24; i < len(data); i += 8 {
		binary.LittleEndian.PutUint64(data[i:], rng.Uint64())
	}
	return data
}

// slotVersion returns the version a slot holds, or an error when it holds
// anything but zeros or one whole version of itself.
func slotVersion(file, slot int, data []byte) (uint32, error) {
	if len(data) < slotSize {
		data = append(data, make([]byte, slotSize-len(data))...)
	}
	v := binary.LittleEndian.Uint32(data[16:])
	if bytes.Equal(data, slotData(file, slot, v)) {
		return v, nil
	}
	if !slices.ContainsFunc(data, func(b byte) bool { return b != 0 }) {
		return 0, nil
	}
	return 0, fmt.Errorf("file %d slot %d holds neither zeros nor one whole version of itself", file, slot)
}

// slotState tracks one slot. Versions only grow. since holds the versions sent
// since the last flush that could be the slot's content after a crash. A
// racy slot is written by two writers at once, so any version sent since
// the last check may win, but never the zeros once a write is certain.
type slotState struct {
	since         []uint32
	next          uint32 // last version handed out
	done, flushed uint32 // last acknowledged, last known durable
	racy          bool
	// cut is set when a truncate over the slot failed without a reply: it
	// may hold zeros as well.
	cut bool
}

// allowed returns the versions a slot may hold. After a quiet run it holds
// its last acknowledged version, after a dropped connection that or a later
// one, and after a kill its last flushed version or a later one.
func (s *slotState) allowed(mode string) []uint32 {
	if s.cut {
		return append([]uint32{0, s.flushed, s.done}, s.since...)
	}
	switch {
	case s.racy:
		certain := s.done
		if mode == "kill" {
			certain = s.flushed
		}
		return slices.DeleteFunc(append([]uint32{s.flushed, s.done}, s.since...), func(v uint32) bool { return v == 0 && certain > 0 })
	case mode == "kill":
		return append([]uint32{s.flushed, s.done}, s.since...)
	case mode == "drop":
		return append([]uint32{s.done}, slices.DeleteFunc(slices.Clone(s.since), func(v uint32) bool { return v < s.done })...)
	default:
		return []uint32{s.done}
	}
}

// macLoad writes bands the way Time Machine does. Writers on two connections
// of the same client keep many 1 MiB WRITEs in flight across several files,
// enough to fill the 256 MiB RAM budget, and some race for the same slots.
// Readers check bytes all the time. Operators mix in FLUSH, full FLUSH,
// CLOSE alone and later in compounds, also of a handle with its own WRITE
// in flight, CREATE, QUERY_INFO and CLOSE compounds, delete on close,
// truncate, rename, conflicting opens and lease breaks.
type macLoad struct {
	t       *testing.T
	f       *fixture
	scratch map[string][]byte
	// flushed hears of each FLUSH of one file that succeeded.
	flushed chan struct{}
	conns   []*macConn
	handles [][]wire.FileID // per connection, one per band with a lease, then the side file
	addrs   []string        // per connection
	slow    []bool          // per connection: its link is slowed on purpose
	// cut is set for a connection the test ends on purpose.
	cut    []atomic.Bool
	leases [][16]byte
	slots  [][]slotState // per band, and the side file last
	// fired holds the operations started once writes were stuck.
	fired   sync.WaitGroup
	seed    uint64
	writers int
	serial  int
	mu      sync.Mutex
	sideMu  sync.Mutex // one operator at a time uses the side file
	// fileFlushes makes the flushing operator flush one file at a time.
	fileFlushes bool
	// failures lets requests fail with a status, as they may while the disk
	// fails, and counts them in failed. Only replies that succeed change the
	// model either way.
	failures bool
	failed   atomic.Int64
}

// loadBands is the number of band files, and loadSlots the slots of each:
// 6 × 64 MiB is well above the 256 MiB RAM budget. Every seventh band slot is
// racy.
const (
	loadBands = 6
	loadSlots = 64
	sideSlots = 8
	sideFile  = loadBands
)

// newMacLoad prepares a load on one connection per address.
func newMacLoad(t *testing.T, f *fixture, rng *rand.Rand, addrs ...string) *macLoad {
	l := &macLoad{t: t, f: f, addrs: addrs, slow: make([]bool, len(addrs)), seed: rng.Uint64(), writers: 16, scratch: map[string][]byte{}, flushed: make(chan struct{}, 1)}
	for range loadBands {
		l.leases = append(l.leases, [16]byte(chaosData(rng, 16)))
		slots := make([]slotState, loadSlots)
		for i := range slots {
			slots[i].racy = i%7 == 0
		}
		l.slots = append(l.slots, slots)
	}
	l.slots = append(l.slots, make([]slotState, sideSlots))
	return l
}

func bandName(b int) string { return fmt.Sprintf("band-%d", b) }

const sideName = "side"

// connect logs in on every address and opens every band, with the same lease
// on each connection, and the side file.
func (l *macLoad) connect(ctx context.Context) {
	l.t.Helper()
	l.conns, l.handles, l.cut = nil, nil, make([]atomic.Bool, len(l.addrs))
	for i, addr := range l.addrs {
		limit := patience
		if l.slow[i] {
			limit = slowPatience
		}
		conn, err := l.f.macConnectWith(ctx, addr, limit)
		if err != nil {
			l.t.Fatal(err)
		}
		var handles []wire.FileID
		for b := range loadBands + 1 {
			options := smbtest.CreateOptions{Request: wire.CreateRequest{Name: sideName, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf}}
			if b < loadBands {
				options.Request.Name = bandName(b)
				options.Lease = &wire.LeaseContext{Version: 2, Key: l.leases[b], State: smbproto.LeaseRead | smbproto.LeaseHandle | smbproto.LeaseWrite}
			}
			id, err := l.open(ctx, conn, options)
			if err != nil {
				l.t.Fatal(err)
			}
			handles = append(handles, id)
		}
		l.conns, l.handles = append(l.conns, conn), append(l.handles, handles)
	}
}

func (l *macLoad) open(ctx context.Context, conn *macConn, options smbtest.CreateOptions) (wire.FileID, error) {
	replies, err := conn.call(ctx, []wire.Message{createMessage(l.t, options)})
	if err != nil {
		return wire.FileID{}, err
	}
	created, err := smbtest.DecodeCreateReply(replies[0])
	return created.Reply.ID, err
}

// closeAll logs off every connection.
func (l *macLoad) closeAll(ctx context.Context) {
	l.t.Helper()
	for c, conn := range l.conns {
		if err := conn.close(ctx); err != nil && (!l.cut[c].Load() || !conn.done()) {
			l.t.Fatal(err)
		}
	}
}

// run writes, reads and operates until stop closes, then waits for every
// request still in flight. A worker stops early when its connection ends.
func (l *macLoad) run(ctx context.Context, stop <-chan struct{}) {
	var wg sync.WaitGroup
	source := func(n uint64) *rand.Rand { return rand.New(rand.NewPCG(l.seed, n)) } //nolint:gosec // Test faults need a replayable source.
	for w := range l.writers {
		rng := source(uint64(w))
		wg.Go(func() { l.write(ctx, rng, w, stop) })
	}
	for r := range 2 {
		rng := source(uint64(100 + r))
		wg.Go(func() { l.read(ctx, rng, stop) })
	}
	// One operator flushes, which waits out a fault. Three others keep
	// operating through it.
	for o := range 4 {
		rng := source(uint64(200 + o))
		wg.Go(func() { l.operate(ctx, rng, o == 0, stop) })
	}
	wg.Wait()
	l.seed++
}

func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// report fails the test for err from connection c, unless the test ended the
// connection on purpose or lets requests fail.
func (l *macLoad) report(c int, format string, err error) {
	var failed statusError
	if l.failures && errors.As(err, &failed) {
		l.failed.Add(1)
		return
	}
	if err == nil || l.cut[c].Load() && l.conns[c].lost() {
		return
	}
	l.t.Errorf(format, err)
}

// cutting tells the load that the test is about to end connection c, or
// every connection with -1.
func (l *macLoad) cutting(c int) {
	for i := range l.cut {
		if c < 0 || i == c {
			l.cut[i].Store(true)
		}
	}
}

// pick chooses a connection that still works, or returns -1.
func (l *macLoad) pick(rng *rand.Rand) int {
	start := rng.IntN(len(l.conns))
	for i := range l.conns {
		if c := (start + i) % len(l.conns); !l.conns[c].done() {
			return c
		}
	}
	return -1
}

// sendSlot writes the next version of a slot through connection c and
// records it.
func (l *macLoad) sendSlot(ctx context.Context, c, file, slot int, extra ...wire.Message) error {
	l.mu.Lock()
	s := &l.slots[file][slot]
	s.next++
	v := s.next
	s.since = append(s.since, v)
	handle := l.handles[c][file]
	l.mu.Unlock()
	messages := append([]wire.Message{writeMessage(l.t, handle, uint64(slot)*slotSize, slotData(file, slot, v))}, extra...) //nolint:gosec // Small test indexes.
	if _, err := l.conns[c].call(ctx, messages, smbproto.StatusFileClosed); err != nil {
		return err
	}
	l.mu.Lock()
	s.done = max(s.done, v)
	l.mu.Unlock()
	return nil
}

// write keeps one WRITE in flight, to the slots writer w owns or, now and
// then, to a racy slot that another writer also writes. Each writer keeps to
// one band, as Time Machine fills one band at a time, and owns every n-th of
// its slots, where n is the number of writers of the band. A FLUSH that waits
// on S3 so stops only its band's writers, and the others fill the RAM budget.
func (l *macLoad) write(ctx context.Context, rng *rand.Rand, w int, stop <-chan struct{}) {
	c, band := w%len(l.conns), w%loadBands
	rank, writers := w/loadBands, (l.writers-band+loadBands-1)/loadBands
	var own []int
	for index := rank; index < loadSlots; index += writers {
		if index%7 != 0 {
			own = append(own, index)
		}
	}
	for !stopped(stop) && !l.conns[c].done() {
		file, index := band, own[rng.IntN(len(own))]
		if rng.IntN(8) == 0 {
			file, index = rng.IntN(loadBands), rng.IntN(loadSlots/7)*7
		}
		l.report(c, fmt.Sprintf("WRITE band %d slot %d: %%v", file, index), l.sendSlot(ctx, c, file, index))
	}
}

// read checks random band slots while everything else goes on. A slot holds
// a version between its last acknowledged one before the READ and the last
// sent after it.
func (l *macLoad) read(ctx context.Context, rng *rand.Rand, stop <-chan struct{}) {
	for !stopped(stop) {
		c := l.pick(rng)
		if c < 0 {
			return
		}
		band, slot := rng.IntN(loadBands), rng.IntN(loadSlots)
		l.mu.Lock()
		low := l.slots[band][slot].done
		l.mu.Unlock()
		replies, err := l.conns[c].call(ctx, []wire.Message{readMessage(l.t, l.handles[c][band], uint64(slot)*slotSize, slotSize)}, smbproto.StatusEndOfFile) //nolint:gosec // Small test indexes.
		if err != nil {
			l.report(c, fmt.Sprintf("READ band %d slot %d: %%v", band, slot), err)
			continue
		}
		got, err := slotVersion(band, slot, readData(l.t, replies[0]))
		l.mu.Lock()
		s := l.slots[band][slot]
		l.mu.Unlock()
		switch {
		case err != nil:
			l.t.Errorf("READ during the load: %v", err)
		case s.racy && (got > s.next || got == 0 && low > 0):
			l.t.Errorf("READ during the load: racy band %d slot %d holds version %d, with %d acknowledged and %d sent", band, slot, got, low, s.next)
		case !s.racy && (got < low || got > s.next):
			l.t.Errorf("READ during the load: band %d slot %d holds version %d, outside %d to %d", band, slot, got, low, s.next)
		}
	}
}

// readData returns the bytes of a READ reply, none at end of file.
func readData(t *testing.T, reply wire.Message) []byte {
	if reply.Header.Status != smbproto.StatusSuccess {
		return nil
	}
	response, err := wire.DecodeReadResponse(reply)
	if err != nil {
		t.Error(err)
	}
	return response.Data
}

// operate runs one mixed operation after another: only FLUSH and full FLUSH
// with flushes, else everything else.
func (l *macLoad) operate(ctx context.Context, rng *rand.Rand, flushes bool, stop <-chan struct{}) {
	for !stopped(stop) {
		c := l.pick(rng)
		if c < 0 {
			return
		}
		op := 2 + rng.IntN(9)
		if flushes {
			op = rng.IntN(2)
			if l.fileFlushes {
				op = 0
			}
		}
		l.report(c, operationNames[op]+": %v", l.operation(ctx, c, rng, op))
	}
}

// operation runs operation op through connection c.
func (l *macLoad) operation(ctx context.Context, c int, rng *rand.Rand, op int) error {
	conn, band := l.conns[c], rng.IntN(loadBands)
	var err error
	switch op {
	case 0:
		err = l.flush(ctx, c, []int{band}, false)
	case 1:
		err = l.flush(ctx, c, nil, true)
	case 2:
		err = l.reopenAndClose(ctx, c, band, false)
	case 3:
		err = l.reopenAndClose(ctx, c, band, true)
	case 4:
		_, err = conn.call(ctx, []wire.Message{
			createMessage(l.t, smbtest.CreateOptions{Request: wire.CreateRequest{Name: bandName(band), DesiredAccess: 0x80, ShareAccess: 7, Disposition: fileOpenIf}}),
			relatedTo(queryInfoMessage(l.t, related)), relatedTo(closeMessage(l.t, related)),
		})
	case 5:
		err = l.churn(ctx, c, rng)
	case 6:
		err = l.truncateSide(ctx, c, rng)
	case 7:
		err = l.renameSide(ctx, c)
	case 8:
		err = l.breakLease(ctx, c, rng, band)
	case 9:
		err = l.closeWhileWriting(ctx, c, rng, band)
	case 10:
		// A conflicting open that denies all sharing must fail at once.
		request := wire.CreateRequest{Name: bandName(band), DesiredAccess: fileAllAccess, ShareAccess: 0, Disposition: fileOpenIf}
		var replies []wire.Message
		if replies, err = conn.call(ctx, []wire.Message{createMessage(l.t, smbtest.CreateOptions{Request: request})}, smbproto.StatusSharingViolation); err == nil && replies[0].Header.Status != smbproto.StatusSharingViolation {
			err = fmt.Errorf("an open denying all sharing of %s succeeded while others have it open", bandName(band))
		}
	}
	return err
}

var operationNames = []string{
	"FLUSH", "full FLUSH", "CLOSE alone", "CLOSE in a compound", "CREATE, QUERY_INFO, CLOSE", "create or delete on close",
	"truncate", "rename", "lease break", "CLOSE behind its own WRITE", "conflicting open",
}

// waitStuck waits until WRITEs to three bands at once are stuck on S3, then
// starts every operation but FLUSH once, each on its own. Only one operator
// flushes, so at least two of those WRITEs wait for room in the RAM budget:
// it is full, and the early uploads that would make room wait on S3.
// loadRound waits for the operations.
func (l *macLoad) waitStuck(ctx context.Context, rng *rand.Rand) {
	l.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		bands := map[int]bool{}
		for c, conn := range l.conns {
			for _, file := range conn.stuckWrites() {
				if band := slices.Index(l.handles[c][:loadBands], file); band >= 0 {
					bands[band] = true
				}
			}
		}
		if len(bands) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			l.t.Error("coverage: writes never got stuck on S3, so the RAM budget never filled")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	for op := 2; op < len(operationNames); op++ {
		c, source := rng.IntN(len(l.conns)), rand.New(rand.NewPCG(rng.Uint64(), uint64(op))) //nolint:gosec // Test faults need a replayable source.
		l.fired.Go(func() {
			l.report(c, operationNames[op]+" while writes were stuck: %v", l.operation(ctx, c, source, op))
		})
	}
}

// flush flushes one file through connection c, or with full every file.
// What was acknowledged before it started is durable once it succeeds.
func (l *macLoad) flush(ctx context.Context, c int, files []int, full bool) error {
	if full {
		files = make([]int, len(l.slots))
		for i := range files {
			files[i] = i
		}
	}
	l.mu.Lock()
	acknowledged := make([][]uint32, len(l.slots))
	for _, file := range files {
		for _, s := range l.slots[file] {
			acknowledged[file] = append(acknowledged[file], s.done)
		}
	}
	l.mu.Unlock()
	if _, err := l.conns[c].call(ctx, []wire.Message{flushMessage(l.t, l.handles[c][files[0]], full)}); err != nil {
		return err
	}
	if !full {
		select {
		case l.flushed <- struct{}{}:
		default:
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, file := range files {
		for i := range l.slots[file] {
			s := &l.slots[file][i]
			if v := acknowledged[file][i]; v > s.flushed {
				s.flushed = v
				// An older write to a racy slot may still overtake v.
				if !s.racy {
					s.since = slices.DeleteFunc(s.since, func(x uint32) bool { return x <= v })
				}
			}
		}
	}
	return nil
}

// reopenAndClose opens a band again and closes it, alone or after an ECHO in
// one compound.
func (l *macLoad) reopenAndClose(ctx context.Context, c, band int, compound bool) error {
	id, err := l.open(ctx, l.conns[c], smbtest.CreateOptions{Request: wire.CreateRequest{Name: bandName(band), DesiredAccess: 0x80, ShareAccess: 7, Disposition: fileOpenIf}})
	if err != nil {
		return err
	}
	messages := []wire.Message{closeMessage(l.t, id)}
	if compound {
		messages = append([]wire.Message{emptyMessage(l.t, wire.Echo, wire.EncodeEchoRequest)}, messages...)
	}
	_, err = l.conns[c].call(ctx, messages)
	return err
}

// closeWhileWriting opens a band again, sends a WRITE of a racy slot and the
// CLOSE of the same handle in one compound, then a READ on the closed
// handle. The CLOSE waits for its own WRITE.
func (l *macLoad) closeWhileWriting(ctx context.Context, c int, rng *rand.Rand, band int) error {
	id, err := l.open(ctx, l.conns[c], smbtest.CreateOptions{Request: wire.CreateRequest{Name: bandName(band), DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf}})
	if err != nil {
		return err
	}
	slot := 7 * rng.IntN(loadSlots/7)
	l.mu.Lock()
	s := &l.slots[band][slot]
	s.next++
	v := s.next
	s.since = append(s.since, v)
	l.mu.Unlock()
	if _, err = l.conns[c].call(ctx, []wire.Message{writeMessage(l.t, id, uint64(slot)*slotSize, slotData(band, slot, v)), closeMessage(l.t, id)}); err != nil { //nolint:gosec // Small test indexes.
		return err
	}
	l.mu.Lock()
	s.done = max(s.done, v)
	l.mu.Unlock()
	_, err = l.conns[c].call(ctx, []wire.Message{readMessage(l.t, id, 0, 4096)}, smbproto.StatusFileClosed)
	return err
}

// churn creates a scratch file with data in one compound, or deletes one on
// close in another.
func (l *macLoad) churn(ctx context.Context, c int, rng *rand.Rand) error {
	l.mu.Lock()
	var names []string
	for name := range l.scratch {
		names = append(names, name)
	}
	l.mu.Unlock()
	slices.Sort(names)
	if len(names) > 0 && rng.IntN(2) == 0 {
		name := names[rng.IntN(len(names))]
		l.mu.Lock()
		delete(l.scratch, name)
		l.mu.Unlock()
		request := wire.CreateRequest{Name: name, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: 1, Options: 0x1000}
		// Another operator may have deleted it first.
		_, err := l.conns[c].call(ctx, []wire.Message{createMessage(l.t, smbtest.CreateOptions{Request: request}), relatedTo(closeMessage(l.t, related))}, smbproto.StatusObjectNameNotFound)
		return err
	}
	l.mu.Lock()
	l.serial++
	name := fmt.Sprintf("scratch-%d", l.serial)
	l.mu.Unlock()
	data := chaosData(rng, 1+rng.IntN(slotSize))
	request := wire.CreateRequest{Name: name, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: 2}
	if _, err := l.conns[c].call(ctx, []wire.Message{
		createMessage(l.t, smbtest.CreateOptions{Request: request}), relatedTo(writeMessage(l.t, related, 0, data)), relatedTo(closeMessage(l.t, related)),
	}); err != nil {
		return err
	}
	l.mu.Lock()
	l.scratch[name] = data
	l.mu.Unlock()
	return nil
}

// truncateSide writes a slot of the side file, then truncates it to a random
// number of slots. A truncate commits at once. One operator at a time owns
// the side file.
func (l *macLoad) truncateSide(ctx context.Context, c int, rng *rand.Rand) error {
	l.sideMu.Lock()
	defer l.sideMu.Unlock()
	if err := l.sendSlot(ctx, c, sideFile, rng.IntN(sideSlots)); err != nil {
		return err
	}
	keep := rng.IntN(sideSlots + 1)
	input := build(l.t, wire.SetInfo, wire.EncodeFileEndOfFileInformation, wire.FileEndOfFileInformation{EndOfFile: uint64(keep) * slotSize}).Body //nolint:gosec // Small test indexes.
	if _, err := l.conns[c].call(ctx, []wire.Message{setInfoMessage(l.t, l.handles[c][sideFile], wire.ClassFileEndOfFile, input)}); err != nil {
		l.mu.Lock()
		for i := keep; i < sideSlots; i++ {
			l.slots[sideFile][i].cut = true
		}
		l.mu.Unlock()
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := keep; i < sideSlots; i++ {
		l.slots[sideFile][i] = slotState{next: l.slots[sideFile][i].next}
	}
	return nil
}

// renameSide moves the side file away and back.
func (l *macLoad) renameSide(ctx context.Context, c int) error {
	l.sideMu.Lock()
	defer l.sideMu.Unlock()
	for _, name := range []string{sideName + "-moved", sideName} {
		input := build(l.t, wire.SetInfo, wire.EncodeFileRenameInformation, wire.FileRenameInformation{Name: name, ReplaceIfExists: true}).Body
		if _, err := l.conns[c].call(ctx, []wire.Message{setInfoMessage(l.t, l.handles[c][sideFile], wire.ClassFileRename, input)}); err != nil {
			return fmt.Errorf("rename the side file to %s: %w", name, err)
		}
	}
	return nil
}

// breakLease opens a band under another lease key, which breaks the band's
// lease. The Mac acknowledges the break, then closes the new open.
func (l *macLoad) breakLease(ctx context.Context, c int, rng *rand.Rand, band int) error {
	lease := wire.LeaseContext{Version: 2, Key: [16]byte(chaosData(rng, 16)), State: smbproto.LeaseRead | smbproto.LeaseHandle | smbproto.LeaseWrite}
	id, err := l.open(ctx, l.conns[c], smbtest.CreateOptions{
		Request: wire.CreateRequest{Name: bandName(band), DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf}, Lease: &lease,
	})
	if err != nil {
		return err
	}
	_, err = l.conns[c].call(ctx, []wire.Message{closeMessage(l.t, id)})
	return err
}

// check reads every slot through the first connection and checks it against
// the model, as slotState.allowed says for mode. The model then takes what
// it found.
func (l *macLoad) check(ctx context.Context, mode string) {
	l.t.Helper()
	conn := l.conns[0]
	type read struct {
		request    *macRequest
		file, slot int
	}
	var reads []read
	for file := range l.slots {
		for slot := range l.slots[file] {
			requests, err := conn.start(ctx, []wire.Message{readMessage(l.t, l.handles[0][file], uint64(slot)*slotSize, slotSize)})
			if err != nil {
				l.t.Fatal(err)
			}
			reads = append(reads, read{requests[0], file, slot})
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range reads {
		reply, err := conn.wait(r.request)
		if err != nil {
			l.t.Fatal(err)
		}
		if status := reply.Header.Status; status != smbproto.StatusSuccess && status != smbproto.StatusEndOfFile {
			l.t.Fatalf("READ file %d slot %d: status %#x", r.file, r.slot, status)
		}
		got, err := slotVersion(r.file, r.slot, readData(l.t, reply))
		if err != nil {
			l.t.Fatal(err)
		}
		s := &l.slots[r.file][r.slot]
		if allowed := s.allowed(mode); !slices.Contains(allowed, got) {
			l.t.Fatalf("after %s, file %d slot %d holds version %d; allowed %v (acknowledged %d, flushed %d)", mode, r.file, r.slot, got, allowed, s.done, s.flushed)
		}
		*s = slotState{next: s.next, done: got, flushed: s.flushed, racy: s.racy}
		if mode == "kill" {
			s.flushed = got
		}
	}
	for name, want := range l.scratch {
		request := wire.CreateRequest{Name: name, DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: 1}
		replies, err := conn.call(ctx, []wire.Message{
			createMessage(l.t, smbtest.CreateOptions{Request: request}), relatedTo(readMessage(l.t, related, 0, slotSize)), relatedTo(closeMessage(l.t, related)),
		}, smbproto.StatusObjectNameNotFound, smbproto.StatusEndOfFile)
		if err != nil {
			l.t.Fatal(err)
		}
		got := readData(l.t, replies[1])
		switch {
		case replies[0].Header.Status == smbproto.StatusSuccess && bytes.Equal(got, want):
		case mode == "kill" && len(got) == 0:
			// Scratch files are closed but never flushed, so a kill may
			// lose them or their bytes.
			delete(l.scratch, name)
		default:
			l.t.Fatalf("after %s, %s holds %d bytes, not its %d (status %#x)", mode, name, len(got), len(want), replies[0].Header.Status)
		}
	}
}

// loadRound runs the load while fault runs on the test goroutine, and for a
// second more, then stops it.
func (l *macLoad) loadRound(ctx context.Context, fault func()) {
	stop := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		l.run(ctx, stop)
	}()
	fault()
	time.Sleep(time.Second)
	close(stop)
	<-ended
	l.fired.Wait()
	for i, conn := range l.conns {
		l.t.Logf("round, connection %d: %s", i, conn.summary())
	}
}
