package smb2

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
	client "github.com/hirochachacha/go-smb2"
)

type observedConn struct {
	net.Conn
	mu          sync.Mutex
	received    []byte
	tamper      atomic.Int32
	authBarrier *sync.WaitGroup
}

func (c *observedConn) Read(b []byte) (int, error) {
	n, e := c.Conn.Read(b)
	c.mu.Lock()
	c.received = append(c.received, b[:n]...)
	c.mu.Unlock()
	return n, e
}
func (c *observedConn) Write(b []byte) (int, error) {
	if len(b) >= 64 && bytes.Equal(b[:4], []byte{0xfe, 'S', 'M', 'B'}) {
		if c.authBarrier != nil && PacketCodec(b).Command() == SMB2_SESSION_SETUP {
			if i := bytes.Index(b, []byte("NTLMSSP\x00\x03\x00\x00\x00")); i >= 0 {
				c.authBarrier.Done()
				c.authBarrier.Wait()
			}
		}
		if mode := c.tamper.Load(); mode != 0 {
			b = append([]byte(nil), b...)
			if mode == 1 || mode == 4 {
				b[48] ^= 0x80
				if mode == 4 {
					binary.LittleEndian.PutUint64(b[24:32], ^uint64(0))
				}
			} else {
				binary.LittleEndian.PutUint32(b[16:20], binary.LittleEndian.Uint32(b[16:20]) & ^uint32(SMB2_FLAGS_SIGNED))
				if mode == 3 {
					binary.LittleEndian.PutUint64(b[24:32], ^uint64(0))
				}
			}
		}
	}
	return c.Conn.Write(b)
}
func startWireServer(t *testing.T, password string) (*Server, net.Listener) {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	fs := newDeleteDispositionFS(vfs.FileTypeDirectory)
	s := NewServer(&ServerConfig{Xatrrs: true}, &NTLMAuthenticator{UserPassword: map[string]string{"backup": password}}, map[string]vfs.VFSFileSystem{"backup": fs})
	go func() { _ = s.ServeListener(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if e := s.ShutdownContext(ctx); e != nil {
			t.Errorf("shutdown: %v", e)
		}
	})
	return s, l
}
func dialWire(l net.Listener, user, password string, barrier *sync.WaitGroup) (*client.Session, *observedConn, error) {
	c, e := net.Dial("tcp", l.Addr().String())
	if e != nil {
		return nil, nil, e
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	observed := &observedConn{Conn: c, authBarrier: barrier}
	d := &client.Dialer{Negotiator: client.Negotiator{RequireMessageSigning: true}, Initiator: &client.NTLMInitiator{User: user, Password: password}}
	session, e := d.Dial(observed)
	return session, observed, e
}
func TestNamedPasswordWireAuthentication(t *testing.T) {
	for _, password := range []string{"fixture-password", ""} {
		t.Run(map[bool]string{true: "named-empty", false: "password"}[password == ""], func(t *testing.T) {
			_, l := startWireServer(t, password)
			session, c, e := dialWire(l, "backup", password, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			share, e := session.Mount("backup")
			if e != nil {
				t.Fatal(e)
			}
			if e = share.Umount(); e != nil {
				t.Fatal(e)
			}
			c.mu.Lock()
			wire := append([]byte(nil), c.received...)
			c.mu.Unlock()
			foundSession, foundSigned := false, false
			for len(wire) >= 4 {
				n := int(binary.BigEndian.Uint32(wire[:4]))
				if len(wire) < 4+n {
					break
				}
				p := PacketCodec(wire[4 : 4+n])
				if p.Command() == SMB2_SESSION_SETUP && p.Status() == 0 {
					r := SessionSetupResponseDecoder(p.Data())
					if r.SessionFlags()&(SMB2_SESSION_FLAG_IS_GUEST|SMB2_SESSION_FLAG_IS_NULL) != 0 {
						t.Fatal("normal account marked guest/null")
					}
					foundSession = true
				}
				if p.Command() == SMB2_TREE_CONNECT && p.Flags()&SMB2_FLAGS_SIGNED != 0 {
					foundSigned = true
				}
				wire = wire[4+n:]
			}
			if !foundSession || !foundSigned {
				t.Fatal("missing normal session or signed tree response")
			}
			for _, bad := range []struct{ u, p string }{{"unknown", password}, {"backup", "wrong-password"}} {
				s, c, e := dialWire(l, bad.u, bad.p, nil)
				if c != nil {
					c.Close()
				}
				if e == nil || s != nil {
					t.Fatal("bad credentials authenticated")
				}
			}
		})
	}
}
func TestConcurrentAuthenticationWire(t *testing.T) {
	_, l := startWireServer(t, "")
	const count = 6
	var barrier, done sync.WaitGroup
	barrier.Add(count)
	for i := 0; i < count; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			session, c, e := dialWire(l, "backup", "", &barrier)
			if c != nil {
				defer c.Close()
			}
			if e != nil {
				t.Error(e)
				return
			}
			share, e := session.Mount("backup")
			if e != nil {
				t.Error(e)
				return
			}
			if e = share.Umount(); e != nil {
				t.Error(e)
			}
		}()
	}
	done.Wait()
}
func TestSigningRejectsTamperedAndUnsignedWire(t *testing.T) {
	for _, mode := range []int32{1, 2, 3, 4} {
		t.Run(map[int32]string{1: "invalid-signature", 2: "missing-signature", 3: "reserved-message-id", 4: "reserved-message-id-tampered"}[mode], func(t *testing.T) {
			_, l := startWireServer(t, "")
			session, c, e := dialWire(l, "backup", "", nil)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			c.tamper.Store(mode)
			if _, e = session.Mount("backup"); e == nil {
				t.Fatal("required signing bypassed")
			}
			// A reserved response MID can confuse the client's request matching;
			// an API error alone is not proof that the server rejected the request.
			c.mu.Lock()
			wire := append([]byte(nil), c.received...)
			c.mu.Unlock()
			for len(wire) >= 4 {
				n := int(binary.BigEndian.Uint32(wire[:4]))
				if len(wire) < 4+n {
					break
				}
				p := PacketCodec(wire[4 : 4+n])
				if p.Command() == SMB2_TREE_CONNECT && p.Status() == 0 {
					t.Fatal("invalid request reached TREE_CONNECT: successful wire response")
				}
				wire = wire[4+n:]
			}
		})
	}
}
