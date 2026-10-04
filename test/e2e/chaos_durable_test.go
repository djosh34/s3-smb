// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestChaosDurableReconnect(t *testing.T) {
	seed := chaos.Seed(t)
	for _, test := range []struct {
		name      string
		operation wire.Command
	}{{"read", wire.Read}, {"write", wire.Write}, {"flush", wire.Flush}} {
		t.Run(test.name, func(t *testing.T) {
			operation := test.operation
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			f, network := durableChaosFixture(ctx, t)
			peer := newDurableChaosPeer(ctx, t, f, smbtest.Session{})
			ledger := chaos.NewLedger()
			random := chaos.Rand(seed, fmt.Sprintf("durable-data-%d", operation))
			var opens []smbtest.RetainedOpen
			for _, name := range []string{"band-a", "band-b"} {
				open := peer.open(ctx, t, name, 120*time.Second, false)
				opens = append(opens, open)
				data := make([]byte, 32*1024)
				for i := range data {
					data[i] = byte(random.Uint32())
				}
				peer.write(ctx, t, ledger, open, 0, data)
			}

			// Neither band is flushed before the drop. Rule 1 protects these
			// acknowledgments, not just data already committed by FLUSH.
			attempt := bytes.Repeat([]byte{byte(random.Uint32())}, 16*1024)
			cut, outage := durableChaosPlan(seed, operation, os.Getenv("S3_SMB_CHECK_MODE") == "gate")
			drainDurableEvents(network)
			t.Logf("cut schedule:\n%s\noutage schedule:\n%s", cut, outage)
			if err := cut.Run(ctx, network, nil); err != nil {
				t.Fatal(err)
			}
			body, err := durableInterruptedRequest(operation, opens[0].ID, attempt)
			if err != nil {
				t.Fatal(err)
			}
			if operation == wire.Write {
				ledger.Attempt(opens[0].Request.Name, 32*1024, attempt)
			}
			if _, err := peer.exchange(ctx, operation, body); err == nil {
				t.Fatal("interrupted operation reported success")
			} else {
				t.Logf("interrupted operation failed visibly: %v", err)
			}
			waitDurableCut(ctx, t, network, *cut[0].Net)
			reconnectCtx, reconnectCancel := context.WithTimeout(ctx, 30*time.Second)
			defer reconnectCancel()
			if err := outage.Run(reconnectCtx, network, nil); err != nil {
				t.Fatal(err)
			}

			conn, err := (&net.Dialer{}).DialContext(reconnectCtx, "tcp", f.clientAddr)
			if err != nil {
				t.Fatal(err)
			}
			client, session, results, err := smbtest.Reconnect(reconnectCtx, conn, peer.session, peer.login, opens)
			if err != nil {
				t.Fatal(err)
			}
			if err := peer.client.Close(); err != nil {
				t.Fatal(err)
			}
			peer.client, peer.session = client, session
			for i, result := range results {
				if result.Reply.ID.Persistent != opens[i].ID.Persistent || result.Reply.ID.Volatile == opens[i].ID.Volatile {
					t.Fatalf("reconnect replaced the durable open: old %+v, new %+v", opens[i].ID, result.Reply.ID)
				}
				if result.Lease == nil || result.Lease.Key != opens[i].Lease.Key || result.Lease.State&smb.LeaseHandle == 0 || result.Durable == nil {
					t.Fatalf("reconnect lost lease or durable grant: %+v", result)
				}
				opens[i].ID, opens[i].Lease = result.Reply.ID, *result.Lease
			}
			if err := ledger.CheckAcknowledged(peer.readFiles(ctx, opens)); err != nil {
				t.Fatal(err)
			}
			// Resume the same work using the returned IDs, including a retry
			// of the interrupted range. No new CREATE replaces either band.
			for _, open := range opens {
				peer.write(ctx, t, ledger, open, 32*1024, attempt)
				peer.flush(ctx, t, ledger, open)
				peer.closeOpen(ctx, t, open.ID)
			}
			share, closeShare := f.share()
			defer closeShare()
			if err := ledger.CheckAcknowledged(chaosRead(share.ReadFile)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChaosDurableExpiry(t *testing.T) {
	seed := chaos.Seed(t)
	gate := os.Getenv("S3_SMB_CHECK_MODE") == "gate"
	timeout, outageLength := time.Second, 1500*time.Millisecond
	if gate {
		// The Mac gives up around 30s. The server still retains its handles
		// until their granted timeout, which is a separate contract.
		timeout, outageLength = 40*time.Second, 42*time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	f, network := durableChaosFixture(ctx, t)
	peer := newDurableChaosPeer(ctx, t, f, smbtest.Session{})
	ledger := chaos.NewLedger()
	band := peer.open(ctx, t, "earlier-band", timeout, false)
	sentinel := peer.open(ctx, t, "expiry-sentinel", timeout, true)
	initialShare, closeInitialShare := f.share()
	entries, listErr := initialShare.ReadDir(".")
	closeInitialShare()
	if listErr != nil {
		t.Fatal(listErr)
	}
	sentinelEntry := func(entry os.FileInfo) bool { return entry.Name() == sentinel.Request.Name }
	if !slices.ContainsFunc(entries, sentinelEntry) {
		t.Fatal("delete-on-close sentinel was not present before the drop")
	}
	random := chaos.Rand(seed, "durable-expiry-data")
	data := make([]byte, 32*1024)
	for i := range data {
		data[i] = byte(random.Uint32())
	}
	peer.write(ctx, t, ledger, band, 0, data)
	peer.flush(ctx, t, ledger, band)
	tail := []byte("acknowledged without flush\n")
	peer.write(ctx, t, ledger, band, uint64(len(data)), tail)
	offset := len(data) + len(tail)

	cut, _ := durableChaosPlan(seed, wire.Write, false)
	drainDurableEvents(network)
	if err := cut.Run(ctx, network, nil); err != nil {
		t.Fatal(err)
	}
	attempt := bytes.Repeat([]byte("unfinished work\n"), 1024)
	ledger.Attempt(band.Request.Name, int64(offset), attempt)
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: band.ID, Offset: uint64(offset), Data: attempt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.exchange(ctx, wire.Write, body); err == nil {
		t.Fatal("cut WRITE reported success")
	}
	waitDurableCut(ctx, t, network, *cut[0].Net)
	outage := chaos.Schedule{{Net: &netfault.Fault{Drop: true}}, {At: outageLength, Net: &netfault.Fault{}}}
	t.Logf("granted timeout %s, outage schedule:\n%s", timeout, outage)
	// Keep the proxy unavailable while the client tries to resume work.
	if err := outage[:1].Run(ctx, network, nil); err != nil {
		t.Fatal(err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", f.clientAddr)
	if err != nil {
		t.Fatal(err)
	}
	failed, _, _, err := smbtest.Reconnect(ctx, conn, peer.session, peer.login, []smbtest.RetainedOpen{band, sentinel})
	if err == nil {
		if closeErr := failed.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("work resumed through an unavailable proxy")
	}
	t.Logf("unavailable work failed visibly: %v", err)
	if err := outage.Run(ctx, network, nil); err != nil {
		t.Fatal(err)
	}

	// Directory enumeration does not open the sentinel or break its H lease.
	// Wait for removal, not just the deadline: expiry cleanup is asynchronous.
	share, closeShare := f.share()
	defer closeShare()
	cleanupDeadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := share.ReadDir(".")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(entries, sentinelEntry) {
			break
		}
		if time.Now().After(cleanupDeadline) {
			t.Fatal("expiry did not apply delete-on-close through normal cleanup")
		}
		time.Sleep(50 * time.Millisecond)
	}
	previous := peer.session
	expiredPeer := newDurableChaosPeer(ctx, t, f, previous)
	for _, open := range []smbtest.RetainedOpen{band, sentinel} {
		message, err := expiredPeer.create(ctx, smbtest.CreateOptions{Request: open.Request, Lease: &open.Lease, Reconnect: &wire.DurableReconnect{ID: open.ID, CreateGUID: open.CreateGUID}})
		if err != nil {
			t.Fatal(err)
		}
		if message.Header.Status != smb.StatusObjectNameNotFound {
			t.Fatalf("expired DH2C status = %#x, want OBJECT_NAME_NOT_FOUND", message.Header.Status)
		}
	}
	if err := ledger.CheckAcknowledged(chaosRead(share.ReadFile)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, share, "next-band", []byte("new backup succeeds\n"))
	ledger.Write("next-band", 0, []byte("new backup succeeds\n"))
	ledger.Flush("next-band")
	if err := ledger.CheckAcknowledged(chaosRead(share.ReadFile)); err != nil {
		t.Fatal(err)
	}
}

func durableChaosFixture(ctx context.Context, t *testing.T) (*fixture, *netfault.Proxy) {
	t.Helper()
	f := newChaosFixture(t, false)
	info, err := buildinfo.ReadFile(f.binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireRaceSmbnextBuild(info.Settings); err != nil {
		t.Fatal(err)
	}
	f.cacheSize = "8 MB"
	d := f.start()
	t.Cleanup(func() {
		d.stop()
		for _, log := range d.logs {
			data, err := os.ReadFile(log.Name())
			if err != nil {
				t.Error(err)
				continue
			}
			if err := chaos.CheckDaemonLog(data); err != nil {
				t.Errorf("daemon generation %d, %s: %v", f.generation, log.Name(), err)
			}
		}
	})
	network, err := netfault.New(ctx, f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := network.Close(); err != nil {
			t.Error(err)
		}
	})
	f.clientAddr = network.Address()
	return f, network
}

func durableChaosPlan(seed uint64, operation wire.Command, gate bool) (chaos.Schedule, chaos.Schedule) {
	random := chaos.Rand(seed, fmt.Sprintf("durable-schedule-%d", operation))
	fault := &netfault.Fault{CutDirection: netfault.ServerToClient, CutAfter: 1}
	if operation == wire.Read {
		fault.CutAfter = int64(256 + random.IntN(8192))
	}
	if operation == wire.Write {
		fault.CutDirection = netfault.ClientToServer
		fault.CutAfter = int64(256 + random.IntN(8192))
	}
	length := time.Duration(100+random.IntN(200)) * time.Millisecond
	if gate {
		length = time.Duration(25_000+random.IntN(4000)) * time.Millisecond
	}
	return chaos.Schedule{{Net: fault}}, chaos.Schedule{{Net: &netfault.Fault{Drop: true}}, {At: length, Net: &netfault.Fault{}}}
}

func TestDurableChaosPlan(t *testing.T) {
	for seed := range uint64(64) {
		for _, operation := range []wire.Command{wire.Read, wire.Write, wire.Flush} {
			for _, gate := range []bool{false, true} {
				cut, outage := durableChaosPlan(seed, operation, gate)
				replayCut, replayOutage := durableChaosPlan(seed, operation, gate)
				if cut.String() != replayCut.String() || outage.String() != replayOutage.String() {
					t.Fatal("seed did not reproduce the schedules")
				}
				if outage[1].At <= 0 || outage[1].At >= 30*time.Second {
					t.Fatalf("short outage exceeded client reconnect window: %s", outage[1].At)
				}
				if gate && outage[1].At < 25*time.Second || !gate && outage[1].At >= time.Second {
					t.Fatalf("wrong duration for gate=%t: %s", gate, outage[1].At)
				}
				fault := *cut[0].Net
				if operation == wire.Write {
					body, err := durableInterruptedRequest(operation, wire.FileID{Persistent: 1, Volatile: 2}, make([]byte, 16*1024))
					if err != nil {
						t.Fatal(err)
					}
					if fault.CutDirection != netfault.ClientToServer || fault.CutAfter <= 4+52+64+48 || fault.CutAfter >= int64(4+52+64+len(body)) {
						t.Fatalf("WRITE cut must interrupt the encrypted data body: %+v", fault)
					}
				} else if fault.CutDirection != netfault.ServerToClient || fault.CutAfter < 1 || fault.CutAfter >= 32*1024 {
					t.Fatalf("READ/FLUSH cut must interrupt the response: %+v", fault)
				}
			}
		}
	}
}

func durableInterruptedRequest(operation wire.Command, id wire.FileID, data []byte) ([]byte, error) {
	switch operation {
	case wire.Read:
		return wire.EncodeReadRequest(wire.ReadRequest{ID: id, Length: 32 * 1024})
	case wire.Write:
		return wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Offset: 32 * 1024, Data: data})
	case wire.Flush:
		return wire.EncodeFlushRequest(wire.FlushRequest{ID: id})
	default:
		return nil, fmt.Errorf("unsupported durable chaos operation %d", operation)
	}
}

func drainDurableEvents(network *netfault.Proxy) {
	for {
		select {
		case _, ok := <-network.Events():
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func waitDurableCut(ctx context.Context, t *testing.T, network *netfault.Proxy, fault netfault.Fault) {
	t.Helper()
	var forwarded int64
	for {
		select {
		case event, ok := <-network.Events():
			if !ok {
				t.Fatal("network proxy closed before the cut")
			}
			if event.Direction == fault.CutDirection {
				forwarded += event.Bytes
			}
			if event.Cut {
				if event.Direction != fault.CutDirection || forwarded != fault.CutAfter || network.DroppedEvents() != 0 {
					t.Fatalf("wrong cut: %+v, forwarded %d, want %+v, dropped %d", event, forwarded, fault, network.DroppedEvents())
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("no byte-cut event:", ctx.Err())
		}
	}
}

// This peer only sequences this scenario's wire requests. Authentication,
// protection, retained identities and reconnect belong to the shared client.
type durableChaosPeer struct {
	client  *smbtest.Client
	session smbtest.Session
	login   smbtest.LoginOptions
}

func newDurableChaosPeer(ctx context.Context, t *testing.T, f *fixture, previous smbtest.Session) *durableChaosPeer {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", f.clientAddr)
	if err != nil {
		t.Fatal(err)
	}
	client, err := smbtest.NewClient(conn)
	if err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	peer := &durableChaosPeer{client: client, login: smbtest.LoginOptions{Share: "TimeMachine", Account: auth.Account{User: "backup", Password: f.password}, Cipher: smb.CipherAES128GCM, Signing: smb.SigningCMAC, ClientGUID: previous.ClientGUID, PreviousSessionID: previous.SessionID}}
	t.Cleanup(func() {
		if err := peer.client.Close(); err != nil {
			t.Error(err)
		}
	})
	peer.session, err = client.Login(ctx, peer.login)
	if err != nil {
		t.Fatal(err)
	}
	return peer
}

func (p *durableChaosPeer) header(command wire.Command) (wire.Header, error) {
	if p.session.Credits == 0 {
		return wire.Header{}, errors.New("no credit for durable chaos request")
	}
	header := wire.Header{Command: command, MessageID: p.session.NextMessageID, SessionID: p.session.SessionID, TreeID: p.session.TreeID, CreditCharge: 1, Credit: 16}
	p.session.NextMessageID++
	p.session.Credits--
	return header, nil
}

func (p *durableChaosPeer) receive(ctx context.Context, header wire.Header) (wire.Message, error) {
	for {
		reply, err := p.client.Receive(ctx)
		if err != nil {
			return wire.Message{}, err
		}
		if len(reply.Messages) != 1 {
			return wire.Message{}, errors.New("expected one durable chaos reply")
		}
		message := reply.Messages[0]
		if message.Header.Command != header.Command || message.Header.MessageID != header.MessageID || message.Header.SessionID != header.SessionID || message.Header.Flags&wire.FlagAsync == 0 && message.Header.TreeID != header.TreeID {
			return wire.Message{}, errors.New("durable chaos reply identity mismatch")
		}
		p.session.Credits += message.Header.Credit
		if message.Header.Status != smb.StatusPending {
			return message, nil
		}
	}
}

func (p *durableChaosPeer) create(ctx context.Context, options smbtest.CreateOptions) (wire.Message, error) {
	header, err := p.header(wire.Create)
	if err != nil {
		return wire.Message{}, err
	}
	if err := p.client.SendCreate(ctx, header, options); err != nil {
		return wire.Message{}, err
	}
	return p.receive(ctx, header)
}

func (p *durableChaosPeer) exchange(ctx context.Context, command wire.Command, body []byte) (wire.Message, error) {
	header, err := p.header(command)
	if err != nil {
		return wire.Message{}, err
	}
	if err := p.client.Send(ctx, []wire.Message{{Header: header, Body: body}}); err != nil {
		return wire.Message{}, err
	}
	message, err := p.receive(ctx, header)
	if err == nil && message.Header.Status != smb.StatusSuccess && !(command == wire.Read && message.Header.Status == smb.StatusEndOfFile) {
		return message, fmt.Errorf("durable chaos command %d status %#x", command, message.Header.Status)
	}
	return message, err
}

func (p *durableChaosPeer) open(ctx context.Context, t *testing.T, name string, timeout time.Duration, deleteOnClose bool) smbtest.RetainedOpen {
	t.Helper()
	identity := byte(p.session.NextMessageID)
	lease := wire.LeaseContext{Version: 2, Key: [16]byte{identity, 1}, State: smb.LeaseRead | smb.LeaseHandle}
	durable := wire.DurableRequest{CreateGUID: [16]byte{identity, 2}, Timeout: uint32(timeout / time.Millisecond)}
	request := wire.CreateRequest{Name: name, DesiredAccess: 0x10000000, ShareAccess: 7, Disposition: 2, Options: 0x40, ImpersonationLevel: 2}
	if deleteOnClose {
		request.Options |= 0x1000
	}
	message, err := p.create(ctx, smbtest.CreateOptions{Request: request, Lease: &lease, Durable: &durable})
	if err != nil {
		t.Fatal(err)
	}
	result, err := smbtest.DecodeCreateReply(message)
	if err != nil {
		t.Fatal(err)
	}
	if result.Lease == nil || result.Lease.Key != lease.Key || result.Lease.State&smb.LeaseHandle == 0 || result.Durable == nil || result.Durable.Timeout != durable.Timeout {
		t.Fatalf("missing durable/H lease grant or wrong timeout: %+v", result)
	}
	return smbtest.RetainedOpen{Request: request, Lease: *result.Lease, ID: result.Reply.ID, CreateGUID: durable.CreateGUID, ClientGUID: p.session.ClientGUID}
}

func (p *durableChaosPeer) write(ctx context.Context, t *testing.T, ledger *chaos.Ledger, open smbtest.RetainedOpen, offset uint64, data []byte) {
	t.Helper()
	ledger.Attempt(open.Request.Name, int64(offset), data)
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: open.ID, Offset: offset, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	message, err := p.exchange(ctx, wire.Write, body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := wire.DecodeWriteResponse(message)
	if err != nil || response.Count != uint32(len(data)) {
		t.Fatalf("WRITE count %d/%d: %v", response.Count, len(data), err)
	}
	ledger.Write(open.Request.Name, int64(offset), data)
}

func (p *durableChaosPeer) flush(ctx context.Context, t *testing.T, ledger *chaos.Ledger, open smbtest.RetainedOpen) {
	t.Helper()
	body, err := wire.EncodeFlushRequest(wire.FlushRequest{ID: open.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.exchange(ctx, wire.Flush, body); err != nil {
		t.Fatal(err)
	}
	ledger.Flush(open.Request.Name)
}

func (p *durableChaosPeer) closeOpen(ctx context.Context, t *testing.T, id wire.FileID) {
	t.Helper()
	body, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.exchange(ctx, wire.Close, body); err != nil {
		t.Fatal(err)
	}
}

func (p *durableChaosPeer) readFiles(ctx context.Context, opens []smbtest.RetainedOpen) chaos.ReadFunc {
	return func(name string) ([]byte, error) {
		for _, open := range opens {
			if open.Request.Name != name {
				continue
			}
			body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: open.ID, Length: 64 * 1024})
			if err != nil {
				return nil, err
			}
			message, err := p.exchange(ctx, wire.Read, body)
			if err != nil {
				return nil, err
			}
			if message.Header.Status == smb.StatusEndOfFile {
				return []byte{}, nil
			}
			response, err := wire.DecodeReadResponse(message)
			return response.Data, err
		}
		return nil, fmt.Errorf("no retained handle for %s", name)
	}
}
