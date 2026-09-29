package smb2

import (
	"net"
	"testing"
	"time"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
)

func TestUnauthenticatedTreeConnectRejectedWire(t *testing.T) {
	_, l := startWireServer(t, "")
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	transport := direct(c)
	if _, err = transport.Write(requestBytes(&TreeConnectRequest{Path: `\\server\backup`})); err != nil {
		t.Fatal(err)
	}
	size, err := transport.ReadSize()
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, size)
	if _, err = transport.Read(b); err != nil {
		t.Fatal(err)
	}
	if status := NtStatus(PacketCodec(b).Status()); status != STATUS_USER_SESSION_DELETED {
		t.Fatalf("unauthenticated tree status %v", status)
	}
}
