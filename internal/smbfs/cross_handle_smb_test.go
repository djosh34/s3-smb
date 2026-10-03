// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	server "github.com/djosh34/s3-smb/internal/smb2/server"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
	client "github.com/hirochachacha/go-smb2"
)

// Inspect only Direct TCP / SMB2 headers; never retain authentication or file
// payloads. The real server verifies the client's signatures. Count commands to
// ensure the regression traverses WRITE, SET_INFO, FLUSH and READ over TCP.
type signedIOConn struct {
	net.Conn
	mu       sync.Mutex
	header   [68]byte
	have     int
	skip     int
	commands map[uint16]int
	unsigned bool
	invalid  bool
}

func (c *signedIOConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	p = p[:n]
	for len(p) > 0 && !c.invalid {
		if c.skip > 0 {
			k := min(len(p), c.skip)
			p, c.skip = p[k:], c.skip-k
			continue
		}
		k := copy(c.header[c.have:], p)
		c.have += k
		p = p[k:]
		if c.have < len(c.header) {
			continue
		}
		length := int(binary.BigEndian.Uint32(c.header[:4]))
		if length < 64 || !bytes.Equal(c.header[4:8], []byte{0xfe, 'S', 'M', 'B'}) {
			c.invalid = true
			break
		}
		command := binary.LittleEndian.Uint16(c.header[16:18])
		switch command {
		case 7, 8, 9, 17: // FLUSH, READ, WRITE, SET_INFO
			c.commands[command]++
			if binary.LittleEndian.Uint32(c.header[20:24])&8 == 0 {
				c.unsigned = true
			}
		}
		c.have, c.skip = 0, length-64
	}
	return n, err
}

func (c *signedIOConn) requireSigned(t *testing.T, commands ...uint16) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, command := range commands {
		if c.commands[command] == 0 {
			t.Errorf("missing wire command %d", command)
		}
	}
	if c.unsigned || c.invalid {
		t.Errorf("wire coverage invalid: unsigned=%v invalid=%v", c.unsigned, c.invalid)
	}
}

func localSignedShare(t *testing.T, f *fixture) (*client.Share, *signedIOConn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewServer(&server.ServerConfig{MaxIOReads: 1, MaxIOWrites: 1},
		&server.NTLMAuthenticator{UserPassword: map[string]string{"backup": ""}},
		map[string]vfs.VFSFileSystem{"backup": f.s})
	go func() { _ = srv.ServeListener(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.ShutdownContext(ctx); err != nil {
			t.Errorf("SMB shutdown: %v", err)
		}
	})
	raw, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn := &signedIOConn{Conn: raw, commands: make(map[uint16]int)}
	dialer := client.Dialer{
		Negotiator: client.Negotiator{RequireMessageSigning: true},
		Initiator:  &client.NTLMInitiator{User: "backup", Password: ""},
	}
	session, err := dialer.Dial(conn)
	if err != nil {
		t.Fatal(err)
	}
	share, err := session.Mount("backup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := share.Umount(); err != nil {
			t.Errorf("SMB unmount: %v", err)
		}
		if err := session.Logoff(); err != nil {
			t.Errorf("SMB logoff: %v", err)
		}
	})
	return share, conn
}

func openSignedFile(t *testing.T, share *client.Share, name string) *client.File {
	t.Helper()
	file, err := share.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("SMB close: %v", err)
		}
	})
	return file
}

// A metadata checkpoint taken after successful FLUSH must be able to recover
// the acknowledged data even when the flush used a different open handle. No
// object-store fault is injected and the writer remains open during the dump.
func TestSignedSMBCrossHandleFlushCheckpoint(t *testing.T) {
	for _, mode := range []string{"cross-handle", "same-handle"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newFixture(t)
			share, conn := localSignedShare(t, fixture)
			writer := openSignedFile(t, share, "band")
			flusher := writer
			if mode == "cross-handle" {
				flusher = openSignedFile(t, share, "band")
			}
			data := []byte("pending")
			if n, err := writer.WriteAt(data, 0); err != nil || n != len(data) {
				t.Fatalf("WRITE: n=%d err=%v", n, err)
			}
			if err := flusher.Sync(); err != nil {
				t.Fatalf("FLUSH: %v", err)
			}
			conn.requireSigned(t, 7, 9)
			var dump bytes.Buffer
			if err := fixture.m.DumpMeta(&dump, meta.RootInode, 1, true, true, false); err != nil {
				t.Fatal(err)
			}
			recovered := fixtureWithStore(t, fixture.store, dump.Bytes())
			recoveryShare, recoveryConn := localSignedShare(t, recovered)
			reader, err := recoveryShare.Open("band")
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			got := make([]byte, len(data))
			n, err := reader.ReadAt(got, 0)
			recoveryConn.requireSigned(t, 8)
			if n != len(data) || err != nil || !bytes.Equal(got, data) {
				t.Fatalf("checkpoint after successful FLUSH: recovered READ n=%d err=%v; want %d acknowledged bytes", n, err, len(data))
			}
		})
	}
}

// This is a wrong-file-results regression, not a reproduction of issue 64's
// historical framing error. All requests are normal signed SMB requests, with
// no storage fault injection. Seven bytes suffice to retain a pending slice.
func TestSignedSMBCrossHandleTruncate(t *testing.T) {
	for _, size := range []int64{0, 3} {
		for _, mode := range []string{"cross-handle", "same-handle", "preflushed"} {
			t.Run(fmt.Sprintf("%s/%d", mode, size), func(t *testing.T) {
				fixture := newFixture(t)
				share, conn := localSignedShare(t, fixture)
				writer := openSignedFile(t, share, "band")
				other := writer
				if mode != "same-handle" {
					// Open B before writing A: a later CREATE can alter the
					// observed cache state, so do not use Share.Truncate.
					other = openSignedFile(t, share, "band")
				}
				data := []byte("pending")
				if n, err := writer.WriteAt(data, 0); err != nil || n != len(data) {
					t.Fatalf("WRITE: n=%d err=%v", n, err)
				}
				if mode == "preflushed" {
					if err := writer.Sync(); err != nil {
						t.Fatalf("pretruncate FLUSH(A): %v", err)
					}
				}
				if err := other.Truncate(size); err != nil {
					t.Fatalf("SET_INFO EOF: %v", err)
				}
				if err := writer.Sync(); err != nil {
					t.Fatalf("FLUSH(A): %v", err)
				}
				got := make([]byte, 16)
				n, err := other.ReadAt(got, 0)
				conn.requireSigned(t, 7, 8, 9, 17)
				// This SMB client's ReadAt returns nil on a nonempty short
				// read. Check EOF separately at the requested new boundary.
				wantErr := error(nil)
				if size == 0 {
					wantErr = io.EOF
				}
				if n != int(size) || err != wantErr || !bytes.Equal(got[:n], data[:size]) {
					t.Fatalf("after successful WRITE/EOF/FLUSH: READ n=%d err=%v; want n=%d err=%v and truncated prefix", n, err, size, wantErr)
				}
				if n, err := other.ReadAt(got[:1], size); n != 0 || err != io.EOF {
					t.Fatalf("READ at truncated EOF: n=%d err=%v", n, err)
				}
			})
		}
	}
}
