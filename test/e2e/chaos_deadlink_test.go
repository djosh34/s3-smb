// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// TestChaosDeadLink drops the packets of a Mac connection with iptables, as
// a crashed or sleeping Mac would. The kernel then acknowledges nothing,
// which a stalled proxy cannot show. The server must notice the dead link
// within about a minute, through keepalive when the link is idle and through
// the user timeout when its replies go unacknowledged, so that the Mac gets
// back in. A 30-second interruption must not end the connection.
func TestChaosDeadLink(t *testing.T) {
	if _, err := exec.LookPath("iptables"); err != nil || os.Geteuid() != 0 {
		t.Skip("needs iptables as root: run scripts/check.sh")
	}
	for _, test := range []struct {
		name string
		busy bool // replies outstanding, so the server's data goes unacknowledged
		drop time.Duration
	}{
		{"idle", false, 0},
		{"replies outstanding", true, 0},
		{"30-second interruption", true, 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) { deadLink(t, test.busy, test.drop) })
	}
}

// deadLink loses the packets of one Mac connection, those of both directions
// or with busy only the server's, and checks that the server notices, or
// with drop that it keeps the connection through a loss that long.
func deadLink(t *testing.T, busy bool, drop time.Duration) {
	f := newFixture(t)
	d := f.start()
	ctx := t.Context()
	mac, err := f.rawLogin(ctx, f.addr, 0)
	if err != nil {
		t.Fatal(err)
	}
	created, status, err := mac.create(ctx, smbtest.CreateOptions{Request: wire.CreateRequest{Name: "file", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf}})
	if err != nil || status != 0 {
		t.Fatal(status, err)
	}
	id := created.Reply.ID
	written := bytes.Repeat([]byte("dead link "), 6000)
	if err = mac.write(ctx, id, 0, written); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(mac.conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	closed := func() int { return bytes.Count(d.read("stderr.log"), []byte(`"msg":"connection closed"`)) }
	before := closed()
	// The server's packets to the Mac are lost. When the link is idle
	// the Mac's are too.
	rules := [][]string{{"-p", "tcp", "--dport", port, "-j", "DROP"}}
	if !busy {
		rules = append(rules, []string{"-p", "tcp", "--sport", port, "-j", "DROP"})
	}
	dropping := false
	lose := func(on bool) {
		action := map[bool]string{true: "-I", false: "-D"}[on]
		for _, rule := range rules {
			iptables(t, append([]string{action, "INPUT"}, rule...)...)
		}
		dropping = on
	}
	lose(true)
	t.Cleanup(func() {
		if dropping {
			lose(false)
		}
	})
	start := time.Now()
	var reads []wire.Header
	if busy {
		for range 16 {
			body, encodeErr := wire.EncodeReadRequest(wire.ReadRequest{ID: id, Length: 60000})
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			request, sendErr := mac.send(ctx, wire.Read, body)
			if sendErr != nil {
				t.Fatal(sendErr)
			}
			reads = append(reads, request)
		}
	}
	if drop > 0 {
		time.Sleep(drop)
		lose(false)
		for _, read := range reads {
			reply, replyErr := mac.reply(ctx, read)
			if replyErr != nil {
				t.Fatalf("after a %v interruption: %v", drop, replyErr)
			}
			response, decodeErr := wire.DecodeReadResponse(reply)
			if reply.Header.Status != 0 || decodeErr != nil || !bytes.Equal(response.Data, written[:60000]) {
				t.Fatalf("after a %v interruption a READ gave status %#x, %d bytes: %v", drop, reply.Header.Status, len(response.Data), decodeErr)
			}
		}
		if got := closed(); got != before {
			t.Fatalf("a %v interruption ended the connection", drop)
		}
		return
	}
	for closed() == before {
		if time.Since(start) > 100*time.Second {
			t.Fatalf("the server did not notice the dead link in %v", time.Since(start))
		}
		time.Sleep(time.Second)
	}
	t.Logf("the server noticed the dead link after %v", time.Since(start).Round(time.Second))
}

// iptables runs iptables with args.
func iptables(t *testing.T, args ...string) {
	t.Helper()
	if output, err := exec.CommandContext(context.WithoutCancel(t.Context()), "iptables", args...).CombinedOutput(); err != nil { //nolint:gosec // The test builds its own rules.
		t.Fatalf("iptables %v: %v\n%s", args, err, output)
	}
}
