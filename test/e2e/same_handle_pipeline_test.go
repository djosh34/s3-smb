// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	smb "github.com/hirochachacha/go-smb2"
)

// These observers retain a bounded header in memory only. No raw bytes, IDs,
// signatures, authentication messages or payloads are written to the report.
// Depth means complete request bytes observed after socket Write returned, less
// final responses observed after socket Read returned. It is not TCP/server
// queue depth. The two directions can be scheduled in either order; early final
// responses are reconciled explicitly rather than inventing an outstanding IO.
type pipelineStream struct {
	Bytes, Frames, SignedIO, UnsignedIO, Invalid, Compounds uint64
	header                                                  [4]byte
	prefix                                                  [112]byte
	haveHeader, haveBody, size                              int
	broken                                                  bool
}

type pipelineMessage struct {
	id             uint64
	file           [16]byte
	length, status uint32
	response       bool
}

func (s *pipelineStream) feed(p []byte, complete func(pipelineMessage)) {
	s.Bytes += uint64(len(p))
	for len(p) > 0 && !s.broken {
		if s.haveHeader < 4 {
			n := copy(s.header[s.haveHeader:], p)
			s.haveHeader += n
			p = p[n:]
			if s.haveHeader != 4 {
				break
			}
			s.size = int(binary.BigEndian.Uint32(s.header[:]))
			if s.header[0] != 0 || s.size < 64 {
				s.Invalid++
				s.broken = true
				break
			}
		}
		n := min(len(p), s.size-s.haveBody)
		if s.haveBody < len(s.prefix) {
			copy(s.prefix[s.haveBody:], p[:n])
		}
		s.haveBody += n
		p = p[n:]
		if s.haveBody != s.size {
			continue
		}
		s.Frames++
		if !bytes.Equal(s.prefix[:4], []byte{0xfe, 'S', 'M', 'B'}) || binary.LittleEndian.Uint16(s.prefix[4:6]) != 64 {
			s.Invalid++
		} else {
			command := binary.LittleEndian.Uint16(s.prefix[12:14])
			flags := binary.LittleEndian.Uint32(s.prefix[16:20])
			if binary.LittleEndian.Uint32(s.prefix[20:24]) != 0 {
				s.Compounds++
			}
			if command == 8 || command == 9 {
				if flags&8 != 0 {
					s.SignedIO++
				} else {
					s.UnsignedIO++
				}
			}
			if command == 9 {
				m := pipelineMessage{id: binary.LittleEndian.Uint64(s.prefix[24:32]), response: flags&1 != 0}
				m.status = binary.LittleEndian.Uint32(s.prefix[8:12])
				if s.size >= 72 {
					m.length = binary.LittleEndian.Uint32(s.prefix[68:72])
				}
				if !m.response {
					if s.size < 112 || int(binary.LittleEndian.Uint16(s.prefix[66:68]))+int(m.length) != s.size {
						s.Invalid++
					} else {
						copy(m.file[:], s.prefix[80:96])
					}
				}
				complete(m)
			}
		}
		s.haveHeader, s.haveBody, s.size = 0, 0, 0
		clear(s.prefix[:])
		clear(s.header[:])
	}
}

func (s *pipelineStream) clean() bool {
	return s.Invalid == 0 && s.Compounds == 0 && s.UnsignedIO == 0 && s.haveHeader == 0 && s.haveBody == 0
}

type pipelineDepth struct {
	Requests, Pending, Finals, FailedFinals, CountMismatch, EarlyFinals int
	RequestedBytes, CompletedBytes, PeakBytes                           uint64
	Peak, Outstanding, DistinctHandles                                  int
	Sizes                                                               map[uint32]int
	ArrivalDepth                                                        map[int]int
	active                                                              map[uint64]uint32
	early                                                               map[uint64]pipelineMessage
	handles                                                             map[[16]byte]bool
	bytes                                                               uint64
}

func newPipelineDepth() *pipelineDepth {
	return &pipelineDepth{Sizes: map[uint32]int{}, ArrivalDepth: map[int]int{}, active: map[uint64]uint32{}, early: map[uint64]pipelineMessage{}, handles: map[[16]byte]bool{}}
}

func (d *pipelineDepth) observe(m pipelineMessage) {
	if !m.response {
		d.Requests++
		d.RequestedBytes += uint64(m.length)
		d.Sizes[m.length]++
		d.handles[m.file] = true
		d.DistinctHandles = len(d.handles)
		if done, ok := d.early[m.id]; ok {
			delete(d.early, m.id)
			if done.status == 0 && done.length != m.length {
				d.CountMismatch++
			}
			return
		}
		d.active[m.id] = m.length
		d.bytes += uint64(m.length)
		d.Outstanding = len(d.active)
		d.Peak = max(d.Peak, d.Outstanding)
		d.PeakBytes = max(d.PeakBytes, d.bytes)
		d.ArrivalDepth[d.Outstanding]++
		return
	}
	if m.status == 0x103 {
		d.Pending++
		return
	}
	d.Finals++
	if m.status != 0 {
		d.FailedFinals++
	} else {
		d.CompletedBytes += uint64(m.length)
	}
	if n, ok := d.active[m.id]; ok {
		delete(d.active, m.id)
		d.bytes -= uint64(n)
		d.Outstanding = len(d.active)
		if m.status == 0 && m.length != n {
			d.CountMismatch++
		}
	} else {
		d.EarlyFinals++
		d.early[m.id] = m
	}
}

type pipelineConn struct {
	net.Conn
	mu             sync.Mutex // metadata only; never held across socket IO
	sent, received pipelineStream
	phase          *pipelineDepth
}

func (c *pipelineConn) observe(p []byte, sending bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &c.received
	if sending {
		s = &c.sent
	}
	s.feed(p, func(m pipelineMessage) {
		if c.phase != nil {
			c.phase.observe(m)
		}
	})
}
func (c *pipelineConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.observe(p[:n], true)
	return n, err
}
func (c *pipelineConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.observe(p[:n], false)
	return n, err
}

func TestSameHandlePipelineObserver(t *testing.T) {
	d := newPipelineDepth()
	for id := range uint64(32) {
		d.observe(pipelineMessage{id: id, length: 1 << 20})
	}
	for id := range uint64(32) {
		d.observe(pipelineMessage{id: id, response: true, status: 0x103})
	}
	if d.Peak != 32 || d.Outstanding != 32 || d.PeakBytes != 32<<20 {
		t.Fatal("pending incorrectly completed IO")
	}
	for id := range uint64(32) {
		d.observe(pipelineMessage{id: id, response: true, length: 1 << 20})
	}
	if d.Outstanding != 0 || d.CompletedBytes != 32<<20 || d.CountMismatch != 0 {
		t.Fatal("final accounting")
	}
	d.observe(pipelineMessage{id: 50, response: true, length: 256 << 10})
	d.observe(pipelineMessage{id: 50, length: 256 << 10})
	if d.EarlyFinals != 1 || len(d.early) != 0 || d.Outstanding != 0 {
		t.Fatal("cross-direction observation ordering")
	}
	// Valid synthetic frames test observer fragmentation only, never network input.
	frame := make([]byte, 4+112+1024)
	binary.BigEndian.PutUint32(frame, uint32(len(frame)-4))
	copy(frame[4:], []byte{0xfe, 'S', 'M', 'B'})
	binary.LittleEndian.PutUint16(frame[8:], 64)
	binary.LittleEndian.PutUint16(frame[16:], 9)
	binary.LittleEndian.PutUint32(frame[20:], 8)
	binary.LittleEndian.PutUint16(frame[70:], 112)
	binary.LittleEndian.PutUint32(frame[72:], 1024)
	for _, fragment := range []int{1, 3, 4, 17, 64, 111, 4096} {
		var s pipelineStream
		d := newPipelineDepth()
		for off := 0; off < len(frame); off += fragment {
			s.feed(frame[off:min(off+fragment, len(frame))], d.observe)
		}
		if !s.clean() || s.Frames != 1 || d.Requests != 1 || d.RequestedBytes != 1024 {
			t.Fatalf("fragment %d", fragment)
		}
	}
}

func pipelinePattern(p []byte, block int) {
	for i := range p {
		p[i] = byte((i*31 + i/257 + block*17) % 251)
	}
}

// Opt-in bounded ordinary same-handle disjoint writes using the unmodified
// repository test client. No socket deadline, stalled reader or security change.
func TestSignedSameHandlePipeline(t *testing.T) {
	if os.Getenv("S3_SMB_SAME_HANDLE_PIPELINE") != "1" {
		t.Skip("opt-in ordinary pipeline control")
	}
	if os.Getenv("S3_SMB_TEST_ARTIFACTS") == "" {
		t.Setenv("S3_SMB_TEST_ARTIFACTS", t.TempDir())
	}
	f := newFixture(t, true)
	f.cacheSize, f.compression, f.interval = "0 MB", "none", "5m"
	d := f.start()
	raw, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	conn := &pipelineConn{Conn: raw}
	defer conn.Close()
	dial := smb.Dialer{MaxCreditBalance: 512, Negotiator: smb.Negotiator{RequireMessageSigning: true}, Initiator: &smb.NTLMInitiator{User: "backup", Password: f.password}}
	session, err := dial.DialContext(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	sessionOpen := true
	defer func() {
		if sessionOpen {
			session.Logoff()
		}
	}()
	share, err := session.Mount("TimeMachine")
	if err != nil {
		t.Fatal(err)
	}
	shareOpen := true
	defer func() {
		if shareOpen {
			share.Umount()
		}
	}()
	type result struct {
		Size, Workers int
		Written, Read int64
		WriteSeconds  float64
		Depth         *pipelineDepth
	}
	var results []result
	for _, size := range []int{256 << 10, 1 << 20} {
		for _, workers := range []int{13, 32} {
			name := fmt.Sprintf("pipeline-%d-%d.bin", size, workers)
			file, e := share.Create(name)
			if e != nil {
				t.Fatal(e)
			}
			phase := newPipelineDepth()
			conn.mu.Lock()
			conn.phase = phase
			conn.mu.Unlock()
			var wg, ready sync.WaitGroup
			ready.Add(workers)
			start := make(chan struct{})
			errs := make([]error, workers)
			written := make([]int, workers)
			for worker := range workers {
				wg.Go(func() {
					p := make([]byte, size)
					pipelinePattern(p, worker)
					ready.Done()
					<-start
					written[worker], errs[worker] = file.WriteAt(p, int64(worker*size))
				})
			}
			ready.Wait()
			began := time.Now()
			close(start)
			wg.Wait()
			r := result{Size: size, Workers: workers, WriteSeconds: time.Since(began).Seconds(), Depth: phase}
			conn.mu.Lock()
			conn.phase = nil
			conn.mu.Unlock()
			for i, e := range errs {
				r.Written += int64(written[i])
				if e != nil || written[i] != size {
					t.Errorf("write size=%d worker=%d count=%d error=%v", size, i, written[i], e)
				}
			}
			if err := file.Sync(); err != nil {
				t.Error(err)
			}
			if err := file.Close(); err != nil {
				t.Error(err)
			}
			file, e = share.Open(name)
			if e != nil {
				t.Fatal(e)
			}
			got, want := make([]byte, size), make([]byte, size)
			for worker := range workers {
				pipelinePattern(want, worker)
				n, e := io.ReadFull(file, got)
				r.Read += int64(n)
				if e != nil || !bytes.Equal(got, want) {
					t.Errorf("readback size=%d worker=%d count=%d equal=%t error=%v", size, worker, n, bytes.Equal(got, want), e)
					break
				}
			}
			var tail [1]byte
			if n, e := file.Read(tail[:]); n != 0 || e != io.EOF {
				t.Errorf("unexpected EOF n=%d error=%v", n, e)
			}
			if err := file.Close(); err != nil {
				t.Error(err)
			}
			results = append(results, r)
			if phase.Outstanding != 0 || len(phase.early) != 0 || phase.DistinctHandles != 1 || phase.CountMismatch != 0 || phase.FailedFinals != 0 || phase.RequestedBytes != uint64(workers*size) || phase.CompletedBytes != phase.RequestedBytes || phase.Requests != phase.Finals {
				t.Error("WRITE accounting mismatch")
			}
			t.Logf("size=%d workers=%d observed_peak=%d peak_bytes=%d requests=%d pending=%d finals=%d early_finals=%d written=%d read=%d seconds=%.3f", size, workers, phase.Peak, phase.PeakBytes, phase.Requests, phase.Pending, phase.Finals, phase.EarlyFinals, r.Written, r.Read, r.WriteSeconds)
		}
	}
	if err := share.Umount(); err != nil {
		t.Error(err)
	}
	shareOpen = false
	if err := session.Logoff(); err != nil {
		t.Error(err)
	}
	sessionOpen = false
	conn.Close()
	d.stop()
	counts := map[string]int{}
	for _, log := range d.logs {
		p, e := os.ReadFile(log.Name())
		if e != nil {
			t.Fatal(e)
		}
		for _, marker := range []string{"short client packet header", "invalid transport format", "unverified packet returned", "signing required", "WARNING: DATA RACE"} {
			counts[marker] += bytes.Count(p, []byte(marker))
		}
	}
	for marker, n := range counts {
		if n != 0 {
			t.Errorf("application count %s=%d", marker, n)
		}
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !conn.sent.clean() || !conn.received.clean() {
		t.Error("invalid/incomplete framing or unsigned IO")
	}
	report := struct {
		Results                        []result
		ClientToServer, ServerToClient pipelineStream
		ApplicationLogs                map[string]int
	}{results, conn.sent, conn.received, counts}
	p, e := json.MarshalIndent(report, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(os.Getenv("S3_SMB_TEST_ARTIFACTS"), "pipeline-summary.json")
	if e := os.WriteFile(path, p, 0600); e != nil {
		t.Fatal(e)
	}
}
