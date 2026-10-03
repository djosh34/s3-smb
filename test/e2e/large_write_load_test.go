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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	smb "github.com/hirochachacha/go-smb2"
)

// loadFrames observes a byte stream, not TCP packets. It keeps only a bounded
// prefix, never emits payloads/signatures, and counts completed frames only.
// It supports the unencrypted, non-compound SMB2 traffic used by this client.
type loadFrames struct {
	Bytes, Frames, Signed, NonzeroSignatures             uint64
	ShortFrames, InvalidType, InvalidSMB, Compounds      uint64
	ReadFrames, WriteFrames, UnsignedIO, ZeroSignatureIO uint64
	WriteBytes                                           uint64
	MaxFrame                                             uint32
	Dialect                                              uint16
	WriteSizes                                           map[uint32]uint64
	Commands                                             map[uint16]uint64
	LengthHistogram                                      map[uint32]uint64
	TransportPrefixPending, BodyPending                  int

	header               [4]byte
	prefix               [112]byte
	headerN, bodyN, size int
	broken               bool
}

func (s *loadFrames) feed(p []byte) {
	s.Bytes += uint64(len(p))
	for len(p) > 0 && !s.broken {
		if s.headerN < 4 {
			n := copy(s.header[s.headerN:], p)
			s.headerN += n
			p = p[n:]
			if s.headerN != 4 {
				break
			}
			if s.header[0] != 0 {
				s.InvalidType++
				s.broken = true
				break
			}
			s.size = int(binary.BigEndian.Uint32(s.header[:]))
			if s.size < 4 {
				s.ShortFrames++
			}
		}
		n := min(len(p), s.size-s.bodyN)
		if s.bodyN < len(s.prefix) {
			copy(s.prefix[s.bodyN:], p[:n])
		}
		s.bodyN += n
		p = p[n:]
		if s.bodyN == s.size {
			s.complete()
			s.headerN, s.bodyN, s.size = 0, 0, 0
			clear(s.prefix[:])
		}
	}
	s.TransportPrefixPending = s.headerN
	s.BodyPending = s.size - s.bodyN
}

func (s *loadFrames) complete() {
	s.Frames++
	s.MaxFrame = max(s.MaxFrame, uint32(s.size))
	if s.LengthHistogram == nil {
		s.LengthHistogram = make(map[uint32]uint64)
	}
	s.LengthHistogram[uint32(s.size)]++
	if s.size < 64 || !bytes.Equal(s.prefix[:4], []byte{0xfe, 'S', 'M', 'B'}) || binary.LittleEndian.Uint16(s.prefix[4:6]) != 64 {
		s.InvalidSMB++
		return
	}
	command := binary.LittleEndian.Uint16(s.prefix[12:14])
	flags := binary.LittleEndian.Uint32(s.prefix[16:20])
	if s.Commands == nil {
		s.Commands = make(map[uint16]uint64)
	}
	s.Commands[command]++
	if binary.LittleEndian.Uint32(s.prefix[20:24]) != 0 {
		s.Compounds++
	}
	signed := flags&8 != 0
	nonzero := !bytes.Equal(s.prefix[48:64], make([]byte, 16))
	if signed {
		s.Signed++
	}
	if nonzero {
		s.NonzeroSignatures++
	}
	if command == 0 && flags&1 != 0 && s.size >= 70 {
		s.Dialect = binary.LittleEndian.Uint16(s.prefix[68:70])
	}
	if command == 8 || command == 9 {
		if !signed {
			s.UnsignedIO++
		}
		if !nonzero {
			s.ZeroSignatureIO++
		}
	}
	if command == 8 {
		s.ReadFrames++
	}
	if command == 9 {
		s.WriteFrames++
		// Both WRITE requests and successful responses have Length/Count at 68.
		if s.size >= 72 && (flags&1 == 0 || binary.LittleEndian.Uint32(s.prefix[8:12]) == 0) {
			n := binary.LittleEndian.Uint32(s.prefix[68:72])
			s.WriteBytes += uint64(n)
			if s.WriteSizes == nil {
				s.WriteSizes = make(map[uint32]uint64)
			}
			s.WriteSizes[n]++
		}
	}
}

func (s *loadFrames) framingErrors() uint64 {
	return s.ShortFrames + s.InvalidType + s.InvalidSMB + s.Compounds
}

type loadConn struct {
	net.Conn
	sentMu, receivedMu sync.Mutex
	sent, received     loadFrames
}

func (c *loadConn) Write(p []byte) (int, error) {
	c.sentMu.Lock()
	defer c.sentMu.Unlock()
	n, err := c.Conn.Write(p)
	c.sent.feed(p[:n])
	return n, err
}
func (c *loadConn) Read(p []byte) (int, error) {
	c.receivedMu.Lock()
	defer c.receivedMu.Unlock()
	n, err := c.Conn.Read(p)
	c.received.feed(p[:n])
	return n, err
}

// Distinct counts: a successful readback must never hide a framing error.
type loadLogCounts struct {
	ShortHeader, InvalidTransport, Race, SignatureFailure int
}

func countLoadLog(p []byte) loadLogCounts {
	return loadLogCounts{
		ShortHeader:      bytes.Count(p, []byte("short client packet header")),
		InvalidTransport: bytes.Count(p, []byte("invalid transport format")),
		Race:             bytes.Count(p, []byte("WARNING: DATA RACE")),
		SignatureFailure: bytes.Count(p, []byte("unverified packet returned")) + bytes.Count(p, []byte("signing required")),
	}
}

func TestLargeWriteLoadCounters(t *testing.T) {
	frame := make([]byte, 4+112+1024)
	binary.BigEndian.PutUint32(frame, uint32(len(frame)-4))
	copy(frame[4:], []byte{0xfe, 'S', 'M', 'B'})
	binary.LittleEndian.PutUint16(frame[8:], 64)
	binary.LittleEndian.PutUint16(frame[16:], 9)
	binary.LittleEndian.PutUint32(frame[20:], 8)
	frame[52] = 1 // synthetic marker, not an authentication signature
	binary.LittleEndian.PutUint32(frame[72:], 1024)
	for _, fragment := range []int{1, 3, 4, 17, 64, 111, 4096} {
		var s loadFrames
		stream := append(append([]byte{}, frame...), frame...)
		for len(stream) > 0 {
			n := min(fragment, len(stream))
			s.feed(stream[:n])
			stream = stream[n:]
		}
		if s.Frames != 2 || s.WriteBytes != 2048 || s.Signed != 2 || s.framingErrors() != 0 || s.TransportPrefixPending != 0 || s.BodyPending != 0 {
			t.Fatalf("fragment=%d incorrect counters", fragment)
		}
	}
	for _, size := range []int{0, 1, 2, 3} {
		p := make([]byte, 4+size)
		binary.BigEndian.PutUint32(p, uint32(size))
		var s loadFrames
		s.feed(p)
		if s.ShortFrames != 1 || s.framingErrors() == 0 {
			t.Fatalf("size=%d short frame not detected", size)
		}
	}
	var bad loadFrames
	bad.feed([]byte{0x85, 0, 0, 0})
	if bad.InvalidType != 1 {
		t.Fatal("invalid type not detected")
	}
	counts := countLoadLog([]byte("short client packet header\ninvalid transport format\nWARNING: DATA RACE\nunverified packet returned"))
	if counts != (loadLogCounts{1, 1, 1, 1}) {
		t.Fatalf("missing exact error counter: %+v", counts)
	}
}

// Opt-in: S3_SMB_LOAD_MIB is MiB per writer (four writers, <= 16 GiB total).
// S3_SMB_LOAD_DURATION paces writes across the requested exposure (<= 10m).
// This is NOT native Time Machine, an APFS-format test, or historical replay.
func TestSignedLargeWriteLoad(t *testing.T) {
	setting := os.Getenv("S3_SMB_LOAD_MIB")
	if setting == "" {
		t.Skip("opt-in sustained load: set S3_SMB_LOAD_MIB per writer")
	}
	mib, err := strconv.Atoi(setting)
	if err != nil || mib < 8 || mib > 4096 || mib%8 != 0 {
		t.Fatal("S3_SMB_LOAD_MIB must be a multiple of 8 in [8,4096]")
	}
	duration := time.Duration(0)
	if value := os.Getenv("S3_SMB_LOAD_DURATION"); value != "" {
		duration, err = time.ParseDuration(value)
		if err != nil || duration < 0 || duration > 10*time.Minute {
			t.Fatal("invalid S3_SMB_LOAD_DURATION (0..10m)")
		}
	}
	tail := 0
	if value := os.Getenv("S3_SMB_LOAD_TAIL"); value != "" {
		tail, err = strconv.Atoi(value)
		if err != nil || tail < 0 || tail > 4096 {
			t.Fatal("S3_SMB_LOAD_TAIL must be in [0,4096]")
		}
	}
	const writers, block = 4, 8 << 20
	perWriter := (int64(mib) << 20) + int64(tail)
	f := newFixture(t, true)
	f.interval = "1m"
	f.cacheSize = os.Getenv("S3_SMB_LOAD_CACHE")
	if f.cacheSize == "" {
		f.cacheSize = "0 MB"
	}
	// Explicit setting: historical Mac used none. Zstd keeps the opt-in zero/
	// periodic-pattern run's stored footprint bounded on shared Linux hosts.
	f.compression = os.Getenv("S3_SMB_LOAD_COMPRESSION")
	if f.compression == "" {
		f.compression = "zstd"
	}
	if os.Getenv("S3_SMB_TEST_ARTIFACTS") == "" {
		t.Setenv("S3_SMB_TEST_ARTIFACTS", t.TempDir())
	}
	d := f.start()
	ctx, cancel := context.WithTimeout(context.Background(), duration+8*time.Minute)
	defer cancel()
	raw, err := net.DialTimeout("tcp", f.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn := &loadConn{Conn: raw}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(duration + 8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	dial := smb.Dialer{MaxCreditBalance: 256, Negotiator: smb.Negotiator{RequireMessageSigning: true}, Initiator: &smb.NTLMInitiator{User: "backup", Password: f.password}}
	session, err := dial.DialContext(ctx, conn)
	if err != nil {
		t.Fatalf("signed session: %v", err)
	}
	share, err := session.Mount("TimeMachine")
	if err != nil {
		t.Fatal(err)
	}
	share = share.WithContext(ctx)

	type outcome struct {
		Writer                    int
		Kind                      string
		Written, Read             int64
		WriteSeconds, ReadSeconds float64
		Error                     string
	}
	results := make([]outcome, writers)
	start := time.Now()
	var wg sync.WaitGroup
	for worker := range writers {
		wg.Go(func() {
			r := &results[worker]
			r.Writer, r.Kind = worker, "zero"
			if worker%2 != 0 {
				r.Kind = "pattern"
			}
			name := fmt.Sprintf("load-%d-%s.bin", worker, r.Kind)
			payload := make([]byte, block)
			file, e := share.Create(name)
			if e != nil {
				r.Error = e.Error()
				return
			}
			defer func() {
				if file != nil {
					file.Close()
				}
			}()
			for off := int64(0); off < perWriter; off += block {
				payload = payload[:min(int64(block), perWriter-off)]
				// Unique worker/block markers in the pattern detect misplacement;
				// zeros remain genuinely zero for the all-zero exposure.
				if r.Kind == "pattern" {
					for i := range payload {
						payload[i] = byte((uint64(i)*31 + uint64(i/257) + uint64(worker)*17 + uint64(off/block)) % 251)
					}
				}
				n, e := file.WriteAt(payload, off)
				if n > 0 {
					r.Written += int64(n)
				}
				if e != nil || n != len(payload) {
					r.Error = fmt.Sprintf("write offset=%d n=%d: %v", off, n, e)
					return
				}
				if duration > 0 {
					target := start.Add(time.Duration(float64(duration) * float64(off+int64(len(payload))) / float64(perWriter)))
					if delay := time.Until(target); delay > 0 {
						select {
						case <-time.After(delay):
						case <-ctx.Done():
							r.Error = ctx.Err().Error()
							return
						}
					}
				}
			}
			if e = file.Sync(); e != nil {
				r.Error = e.Error()
				return
			}
			if e = file.Close(); e != nil {
				r.Error = e.Error()
				return
			}
			file = nil
			r.WriteSeconds = time.Since(start).Seconds()
		})
	}
	wg.Wait()
	writeSeconds := time.Since(start).Seconds()
	// Reopen every file after all writers have flushed/closed; stream every byte
	// over the same signed session instead of retaining multi-GiB expected data.
	for worker := range writers {
		r := &results[worker]
		if r.Error != "" {
			continue
		}
		readStart := time.Now()
		func() {
			file, e := share.Open(fmt.Sprintf("load-%d-%s.bin", worker, r.Kind))
			if e != nil {
				r.Error = e.Error()
				return
			}
			defer file.Close()
			got, want := make([]byte, block), make([]byte, block)
			for off := int64(0); off < perWriter; off += block {
				if r.Kind == "pattern" {
					for i := range want {
						want[i] = byte((uint64(i)*31 + uint64(i/257) + uint64(worker)*17 + uint64(off/block)) % 251)
					}
				}
				length := min(int64(block), perWriter-off)
				n, e := io.ReadFull(file, got[:length])
				r.Read += int64(n)
				if e != nil || !bytes.Equal(got[:length], want[:length]) {
					r.Error = fmt.Sprintf("readback offset=%d n=%d equal=%t: %v", off, n, bytes.Equal(got[:length], want[:length]), e)
					return
				}
			}
			var tail [1]byte
			if n, e := file.Read(tail[:]); n != 0 || e != io.EOF {
				r.Error = fmt.Sprintf("unexpected tail n=%d: %v", n, e)
			}
		}()
		r.ReadSeconds = time.Since(readStart).Seconds()
	}
	if err := share.Umount(); err != nil {
		t.Errorf("unmount: %v", err)
	}
	if err := session.Logoff(); err != nil {
		t.Errorf("logoff: %v", err)
	}
	conn.Close()
	d.stop()

	var logs loadLogCounts
	for _, file := range d.logs {
		p, e := os.ReadFile(file.Name())
		if e != nil {
			t.Fatal(e)
		}
		c := countLoadLog(p)
		logs.ShortHeader += c.ShortHeader
		logs.InvalidTransport += c.InvalidTransport
		logs.Race += c.Race
		logs.SignatureFailure += c.SignatureFailure
	}
	conn.sentMu.Lock()
	defer conn.sentMu.Unlock()
	conn.receivedMu.Lock()
	defer conn.receivedMu.Unlock()
	report := struct {
		Writers                                      int
		BytesPerWriter                               int64
		RequestedSeconds, WriteSeconds, TotalSeconds float64
		Compression, CacheSize                       string
		EncryptionAtRest, RequireMessageSigning      bool
		Results                                      []outcome
		ClientToServer, ServerToClient               loadFrames
		ApplicationLogs                              loadLogCounts
	}{writers, perWriter, duration.Seconds(), writeSeconds, time.Since(start).Seconds(), f.compression, f.cacheSize, true, true, results, conn.sent, conn.received, logs}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("S3_SMB_TEST_ARTIFACTS"), strings.ReplaceAll(t.Name(), "/", "-"), "load-summary.json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("summary=%s bytes_written_target=%d write_duration=%.3fs total_duration=%.3fs concurrency=%d", path, perWriter*writers, writeSeconds, report.TotalSeconds, writers)
	for _, r := range results {
		if r.Error != "" || r.Written != perWriter || r.Read != perWriter {
			t.Errorf("integrity writer=%d written=%d read=%d error=%s", r.Writer, r.Written, r.Read, r.Error)
		}
	}
	if logs != (loadLogCounts{}) {
		t.Errorf("application error counts: %+v", logs)
	}
	for name, s := range map[string]*loadFrames{"client_to_server": &conn.sent, "server_to_client": &conn.received} {
		t.Logf("%s bytes=%d frames=%d write_frames=%d read_frames=%d signed=%d framing_errors=%d", name, s.Bytes, s.Frames, s.WriteFrames, s.ReadFrames, s.Signed, s.framingErrors())
		if s.framingErrors() != 0 || s.TransportPrefixPending != 0 || s.BodyPending != 0 {
			t.Errorf("%s invalid/incomplete framing", name)
		}
		if s.UnsignedIO != 0 || s.ZeroSignatureIO != 0 {
			t.Errorf("%s unsigned IO=%d zero_signature_IO=%d", name, s.UnsignedIO, s.ZeroSignatureIO)
		}
		if s.WriteBytes != uint64(perWriter*writers) {
			t.Errorf("%s wire WRITE bytes=%d want=%d", name, s.WriteBytes, perWriter*writers)
		}
	}
}
