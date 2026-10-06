// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	smbproto "github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// The break tests try to break the server the way a Mac backup can: many
// requests in flight on one connection, faults at any moment, and macOS's
// patience instead of minutes. macOS fails a request that gets neither a
// final reply nor STATUS_PENDING within 2 minutes, and drops the data written
// through it while Time Machine reports success. A healthy server sends
// STATUS_PENDING after 5 ms, so the tests allow 3 seconds, also in the gate.
// A link that a test slows on purpose gets 45 seconds, for stalls of up to
// 30. macOS also fails a request 2 minutes after its last STATUS_PENDING, so
// a request that waits longer must get it again: the tests allow 1 minute.
const (
	patience     = 3 * time.Second
	slowPatience = 45 * time.Second
	waitPatience = time.Minute
)

// related is the FileID of a compound member that uses the previous
// member's open.
var related = wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}

// macConn is one Mac connection with many requests in flight. A receiver
// routes each reply to its request, and a watchdog fails the test when a
// request outlasts the patience without a reply. Any goroutine may send.
type macConn struct {
	t        *testing.T
	client   *smbtest.Client
	credit   *sync.Cond
	inFlight map[uint64]*macRequest
	breaks   chan wire.LeaseBreakNotification
	ended    chan struct{}
	err      error // why the connection ended, set before ended closes
	session  smbtest.Session
	patience time.Duration
	next     uint64
	credits  int
	// For the round summary: requests sent, how many went async, and the
	// slowest first reply.
	sent, pending int
	slowest       time.Duration
	mu            sync.Mutex
	// sendMu keeps the frames in the order their message IDs were given.
	sendMu sync.Mutex
	// left is set once a LOGOFF is sent: the server ends a connection that
	// sends on a session it no longer knows.
	left bool
}

// errLeft is a request after the connection's LOGOFF.
var errLeft = errors.New("the session has logged off")

// A request is queued until its frame is on the wire, then sent. STATUS_PENDING
// for the first member of a compound covers the whole compound: the server
// answers the rest in one chain after it, as macOS reads it.
type macRequest struct {
	queued, sent time.Time
	interim      time.Time // the last STATUS_PENDING, of its compound too
	reply        chan wire.Message
	compound     []*macRequest // every member, this one too
	id           uint64
	file         wire.FileID // of a WRITE
	command      wire.Command
	pending      bool // got STATUS_PENDING, or its compound did
	late         bool // already reported
}

// macConnect logs in at addr as the Mac, with the 3-second patience. The
// connection lives until ctx ends, the server drops it or the test ends.
func (f *fixture) macConnect(ctx context.Context, addr string) (*macConn, error) {
	return f.macConnectWith(ctx, addr, patience)
}

// macConnectWith logs in with a patience of its own.
func (f *fixture) macConnectWith(ctx context.Context, addr string, limit time.Duration) (*macConn, error) {
	mac, err := f.rawLogin(ctx, addr, 0)
	if err != nil {
		return nil, err
	}
	c := &macConn{
		t: f.t, client: mac.client, session: mac.session, patience: limit, next: mac.session.NextMessageID, credits: int(mac.session.Credits),
		inFlight: make(map[uint64]*macRequest), breaks: make(chan wire.LeaseBreakNotification, 256), ended: make(chan struct{}),
	}
	c.credit = sync.NewCond(&c.mu)
	go c.receive(ctx)
	go c.watch()
	go c.acknowledgeBreaks(ctx)
	go c.keepAlive(ctx)
	return c, nil
}

// otherClientRefused logs in at addr as another client, which the server
// must refuse while the Mac is connected.
func (f *fixture) otherClientRefused(ctx context.Context, addr string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	client, err := smbtest.NewClient(conn)
	if err != nil {
		return errors.Join(err, conn.Close())
	}
	_, err = client.Login(ctx, smbtest.LoginOptions{
		Share: "TimeMachine", Account: auth.Account{User: "backup", Password: f.password}, ClientGUID: [16]byte{'o', 't', 'h', 'e', 'r'},
		Cipher: smbproto.CipherAES128GCM, Signing: smbproto.SigningGMAC,
	})
	closeErr := client.Close()
	switch {
	case err == nil:
		return errors.Join(errors.New("another client logged in while the Mac is connected"), closeErr)
	case !strings.Contains(err.Error(), fmt.Sprintf("%#x", smbproto.StatusRequestNotAccepted)):
		return errors.Join(fmt.Errorf("another client was refused for the wrong reason: %w", err), closeErr)
	}
	return nil
}

// start sends one compound and returns its requests. Members must set
// Command, CreditCharge for large I/O and FlagRelated where needed.
func (c *macConn) start(ctx context.Context, messages []wire.Message) ([]*macRequest, error) {
	charge := 0
	for i := range messages {
		messages[i].Header.CreditCharge = max(messages[i].Header.CreditCharge, 1)
		charge += int(messages[i].Header.CreditCharge)
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.mu.Lock()
	for c.credits < charge && c.err == nil {
		c.credit.Wait()
	}
	if c.err != nil || c.left {
		err := c.err
		if err == nil {
			err = errLeft
		}
		c.mu.Unlock()
		return nil, err
	}
	c.credits -= charge
	requests := make([]*macRequest, len(messages))
	now := time.Now()
	for i := range messages {
		h := &messages[i].Header
		h.MessageID, h.SessionID, h.TreeID, h.Credit = c.next, c.session.SessionID, c.session.TreeID, 64
		c.next += uint64(h.CreditCharge)
		requests[i] = &macRequest{id: h.MessageID, command: h.Command, queued: now, reply: make(chan wire.Message, 1), compound: requests}
		c.inFlight[h.MessageID] = requests[i]
		c.left = c.left || h.Command == wire.Logoff
		if h.Command == wire.Write {
			if write, err := wire.DecodeWriteRequest(messages[i]); err == nil {
				requests[i].file = write.ID
			}
		}
	}
	c.sent += len(messages)
	c.mu.Unlock()
	err := c.client.Send(ctx, messages)
	c.mu.Lock()
	for _, r := range requests {
		r.sent = time.Now()
	}
	c.mu.Unlock()
	return requests, err
}

// wait returns the final reply to r, or why the connection ended first.
func (c *macConn) wait(r *macRequest) (wire.Message, error) {
	select {
	case m := <-r.reply:
		return m, nil
	case <-c.ended:
		select {
		case m := <-r.reply:
			return m, nil
		default:
			return wire.Message{}, c.err
		}
	}
}

// call sends one compound and returns its final replies. A member that does
// not succeed is an error, unless its status is in allowed.
func (c *macConn) call(ctx context.Context, messages []wire.Message, allowed ...smbproto.Status) ([]wire.Message, error) {
	requests, err := c.start(ctx, messages)
	if err != nil {
		return nil, err
	}
	replies := make([]wire.Message, len(requests))
	var failed error
	for i, r := range requests {
		if replies[i], err = c.wait(r); err != nil {
			return nil, err
		}
		if status := replies[i].Header.Status; status != smbproto.StatusSuccess && !containsStatus(allowed, status) {
			failed = errors.Join(failed, statusError{command: r.command, status: status})
		}
	}
	return replies, failed
}

// statusError is a request the server answered with a failure.
type statusError struct {
	command wire.Command
	status  smbproto.Status
}

func (e statusError) Error() string { return fmt.Sprintf("%v: status %#x", e.command, e.status) }

func containsStatus(statuses []smbproto.Status, status smbproto.Status) bool {
	for _, s := range statuses {
		if s == status {
			return true
		}
	}
	return false
}

func (c *macConn) receive(ctx context.Context) {
	defer close(c.ended)
	for {
		reply, err := c.client.Receive(ctx)
		for _, b := range c.client.TakeLeaseBreaks() {
			select {
			case c.breaks <- b:
			default:
				c.t.Error("too many lease breaks waiting for an acknowledgment")
			}
		}
		c.mu.Lock()
		if err != nil {
			c.err = fmt.Errorf("connection ended: %w", err)
			c.credit.Broadcast()
			c.mu.Unlock()
			return
		}
		for j, m := range reply.Messages {
			c.credits += int(m.Header.Credit)
			r := c.inFlight[m.Header.MessageID]
			if r == nil {
				c.t.Errorf("reply to message %d, which is not in flight", m.Header.MessageID)
				continue
			}
			c.firstReply(r)
			if !whole(reply.Messages, j, r) {
				c.t.Errorf("%v message %d: its compound came back split, which macOS cannot read", r.command, r.id)
			}
			if m.Header.Status == smbproto.StatusPending && m.Header.Flags&wire.FlagAsync != 0 {
				r.pending = true
				for _, member := range r.compound {
					member.pending, member.interim = true, time.Now()
				}
				c.pending++
				continue
			}
			delete(c.inFlight, m.Header.MessageID)
			r.reply <- m
		}
		c.credit.Broadcast()
		c.mu.Unlock()
	}
}

// firstReply times a reply to r if it is the first. The caller holds c.mu.
func (c *macConn) firstReply(r *macRequest) {
	if r.pending || r.sent.IsZero() {
		return
	}
	age := time.Since(r.sent)
	c.slowest = max(c.slowest, age)
	// The watchdog looks only every 250 ms.
	if !r.late && age > c.patience {
		r.late = true
		c.t.Errorf("%v message %d got its first reply after %v: macOS would have failed it and dropped its data", r.command, r.id, age)
	}
}

// whole reports whether the reply at index j of a frame comes as macOS reads
// it: an interim reply to the first member of its compound alone, or the
// final replies to every member together and in order.
func whole(messages []wire.Message, j int, r *macRequest) bool {
	if len(r.compound) == 1 {
		return true
	}
	if messages[j].Header.Status == smbproto.StatusPending && messages[j].Header.Flags&wire.FlagAsync != 0 {
		return r == r.compound[0] && len(messages) == 1
	}
	start := j - slices.Index(r.compound, r)
	if start < 0 || start+len(r.compound) > len(messages) {
		return false
	}
	for i, member := range r.compound {
		if m := messages[start+i]; m.Header.MessageID != member.id || m.Header.Status == smbproto.StatusPending && m.Header.Flags&wire.FlagAsync != 0 {
			return false
		}
	}
	return true
}

// watch fails the test for every request that outlasts the patience with
// neither a final reply nor STATUS_PENDING.
func (c *macConn) watch() {
	limit := c.patience
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.ended:
			return
		case <-ticker.C:
		}
		c.mu.Lock()
		for id, r := range c.inFlight {
			switch {
			case r.late:
			case r.pending && time.Since(r.interim) > waitPatience:
				r.late = true
				c.t.Errorf("%v message %d got no reply for %v after its STATUS_PENDING: macOS fails it 2 minutes after the last one and drops its data", r.command, id, waitPatience)
			case r.pending:
			case r.sent.IsZero() && time.Since(r.queued) > limit:
				r.late = true
				c.t.Errorf("%v message %d could not be sent within %v: the server stopped reading", r.command, id, limit)
			case !r.sent.IsZero() && time.Since(r.sent) > limit:
				r.late = true
				c.t.Errorf("%v message %d got neither a reply nor STATUS_PENDING within %v: macOS would fail it and drop its data", r.command, id, limit)
			}
		}
		c.mu.Unlock()
	}
}

// acknowledgeBreaks acknowledges each lease break that asks for it, as the
// Mac does.
func (c *macConn) acknowledgeBreaks(ctx context.Context) {
	for {
		select {
		case <-c.ended:
			return
		case b := <-c.breaks:
			if b.Flags&1 == 0 {
				continue
			}
			ack := build(c.t, wire.OplockBreak, wire.EncodeLeaseBreakRequest, wire.LeaseBreakRequest{Key: b.Key, State: b.NewState})
			// The lease may have gone with its open; that is no failure.
			if _, err := c.call(ctx, []wire.Message{ack}, smbproto.StatusUnsuccessful); err != nil && !c.done() && !errors.Is(err, errLeft) {
				c.t.Errorf("lease break acknowledgment: %v", err)
			}
		}
	}
}

// keepAlive sends an ECHO every 100 ms. Its replies keep lease breaks
// flowing, and an ECHO that waits behind a stalled request is caught too.
func (c *macConn) keepAlive(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.ended:
			return
		case <-ticker.C:
		}
		if _, err := c.start(ctx, []wire.Message{emptyMessage(c.t, wire.Echo, wire.EncodeEchoRequest)}); err != nil {
			return
		}
	}
}

// stuckWrites returns the files of the WRITEs that went async more than a
// second ago and still wait.
func (c *macConn) stuckWrites() []wire.FileID {
	c.mu.Lock()
	defer c.mu.Unlock()
	var files []wire.FileID
	for _, r := range c.inFlight {
		if r.command == wire.Write && r.pending && time.Since(r.sent) > time.Second {
			files = append(files, r.file)
		}
	}
	return files
}

// waiting reports whether r still waits for its final reply.
func (c *macConn) waiting(r *macRequest) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight[r.id] == r
}

// summary returns the counts since the last summary, and resets them.
func (c *macConn) summary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := fmt.Sprintf("%d requests, %d went async, slowest first reply %v, %d in flight", c.sent, c.pending, c.slowest, len(c.inFlight))
	c.sent, c.pending, c.slowest = 0, 0, 0
	return s
}

// lost reports whether the connection has ended, waiting a moment for its
// receiver to see what a failed send already saw.
func (c *macConn) lost() bool {
	select {
	case <-c.ended:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// done reports whether the connection has ended.
func (c *macConn) done() bool {
	select {
	case <-c.ended:
		return true
	default:
		return false
	}
}

// close logs off and closes the connection.
func (c *macConn) close(ctx context.Context) error {
	_, err := c.call(ctx, []wire.Message{emptyMessage(c.t, wire.Logoff, wire.EncodeLogoffRequest)})
	return errors.Join(err, c.client.Close())
}

// build encodes one request. Encoding valid values cannot fail, and workers
// call it off the test goroutine, so a failure is only reported.
func build[T any](t *testing.T, command wire.Command, encode func(T) ([]byte, error), v T) wire.Message {
	t.Helper()
	body, err := encode(v)
	if err != nil {
		t.Error(err)
	}
	return wire.Message{Header: wire.Header{Command: command}, Body: body}
}

// relatedTo marks m as related to the previous member.
func relatedTo(m wire.Message) wire.Message {
	m.Header.Flags |= wire.FlagRelated
	return m
}

func createMessage(t *testing.T, options smbtest.CreateOptions) wire.Message {
	t.Helper()
	m, err := smbtest.CreateMessage(wire.Header{}, options)
	if err != nil {
		t.Error(err)
	}
	return m
}

func writeMessage(t *testing.T, id wire.FileID, offset uint64, data []byte) wire.Message {
	m := build(t, wire.Write, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Offset: offset, Data: data})
	m.Header.CreditCharge = creditCharge(len(data))
	return m
}

func readMessage(t *testing.T, id wire.FileID, offset uint64, length int) wire.Message {
	m := build(t, wire.Read, wire.EncodeReadRequest, wire.ReadRequest{ID: id, Offset: offset, Length: uint32(length)}) //nolint:gosec // Reads are at most 1 MiB.
	m.Header.CreditCharge = creditCharge(length)
	return m
}

func closeMessage(t *testing.T, id wire.FileID) wire.Message {
	return build(t, wire.Close, wire.EncodeCloseRequest, wire.CloseRequest{ID: id})
}

// flushMessage flushes id, or every file with full, as Apple's F_FULLFSYNC
// does.
func flushMessage(t *testing.T, id wire.FileID, full bool) wire.Message {
	request := wire.FlushRequest{ID: id}
	if full {
		request.Reserved1 = 0xffff
	}
	return build(t, wire.Flush, wire.EncodeFlushRequest, request)
}

func setInfoMessage(t *testing.T, id wire.FileID, class wire.FileInfoClass, input []byte) wire.Message {
	return build(t, wire.SetInfo, wire.EncodeSetInfoRequest, wire.SetInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(class), Input: input})
}

func queryInfoMessage(t *testing.T, id wire.FileID) wire.Message {
	return build(t, wire.QueryInfo, wire.EncodeQueryInfoRequest, wire.QueryInfoRequest{ID: id, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileStandard), OutputLength: 1024})
}

func emptyMessage(t *testing.T, command wire.Command, encode func(wire.EmptyRequest) ([]byte, error)) wire.Message {
	return build(t, command, encode, wire.EmptyRequest{})
}

// creditCharge is the credits of one I/O request of n bytes.
func creditCharge(n int) uint16 {
	return uint16(max(1, (n+(64<<10)-1)/(64<<10))) //nolint:gosec // I/O is at most 1 MiB.
}
