// Modified for s3-smb, 2026. See docs/vendored.md.

package smb2

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/smb2"
)

type requestResponse struct {
	msgId         uint64
	creditRequest uint16
	pkt           []byte // request packet
	session       *session
	compCtx       *compoundContext
	ctx           context.Context
	recv          chan []byte
	err           error
}

type outstandingRequests struct {
	m        sync.Mutex
	s        chan uint64
	requests map[uint64]*requestResponse
}

func newOutstandingRequests() *outstandingRequests {
	return &outstandingRequests{
		requests: make(map[uint64]*requestResponse, 0),
		s:        make(chan uint64),
	}
}

func (r *outstandingRequests) set(msgId uint64, rr *requestResponse) {
	r.m.Lock()

	r.requests[msgId] = rr
	r.m.Unlock()

	select {
	case r.s <- msgId:
	case <-rr.ctx.Done():
		r.m.Lock()
		delete(r.requests, msgId)
		r.m.Unlock()
	}
}

func (r *outstandingRequests) pop(ctx context.Context) ([]byte, *session, *compoundContext, error) {
	var msgId uint64

again:
	select {
	case <-ctx.Done():
		return nil, nil, nil, &ContextError{Err: ctx.Err()}
	case msgId = <-r.s:
		break
	}

	r.m.Lock()
	rr := r.requests[msgId]
	delete(r.requests, msgId)
	r.m.Unlock()
	if rr == nil {
		// Cancel message
		goto again
	}
	return rr.pkt, rr.session, rr.compCtx, nil
}

func (r *outstandingRequests) shutdown(err error) {
	r.m.Lock()
	defer r.m.Unlock()

	for _, rr := range r.requests {
		rr.err = err
		close(rr.recv)
	}
}

type conn struct {
	t transport

	session                   *session
	sessions                  map[uint64]*session
	outstandingRequests       *outstandingRequests
	sequenceWindow            uint64
	dialect                   uint16
	maxTransactSize           uint32
	maxReadSize               uint32
	maxWriteSize              uint32
	requireSigning            bool
	capabilities              uint32
	preauthIntegrityHashId    uint16
	preauthIntegrityHashValue [64]byte
	cipherId                  uint16
	hashId                    uint16
	posixExtensions           bool

	account *account

	rdone chan struct{}
	wdone chan struct{}
	write chan []byte
	werr  chan error

	m           sync.Mutex
	ioWG        sync.WaitGroup
	lockWG      sync.WaitGroup
	transportWG sync.WaitGroup
	spnego      *spnegoServer

	err error

	// gssNegotiateToken []byte
	// serverGuid        [16]byte
	// clientGuid        [16]byte

	_useSession int32 // receiver use session?

	ctx         context.Context
	cancel      context.CancelFunc
	serverCtx   *Server
	serverState ConnState

	treeMapByName map[string]treeOps
	treeMapById   map[uint32]treeOps
}

func (conn *conn) shutdown() {
	conn.cancel()
	// Use non-blocking sends since shutdown may be called multiple
	// times (from runReciever exit and from Server.Shutdown).
	select {
	case conn.wdone <- struct{}{}:
	default:
	}
	select {
	case conn.rdone <- struct{}{}:
	default:
	}
	conn.t.Close()
}

func (conn *conn) useSession() bool {
	return atomic.LoadInt32(&conn._useSession) != 0
}

func (conn *conn) enableSession() {
	atomic.StoreInt32(&conn._useSession, 1)
}

func (conn *conn) resetSession() {
	atomic.StoreInt32(&conn._useSession, 0)
}

func (conn *conn) registerSession(s *session) {
	conn.sessions[s.sessionId] = s
	conn.session = s
}

func (conn *conn) lookupSession(sessionId uint64) *session {
	if conn.session != nil && conn.session.sessionId == sessionId {
		return conn.session
	}
	return conn.sessions[sessionId]
}

func (conn *conn) encodePacket(req Packet, tc *treeConn, ctx context.Context) ([]byte, error) {
	var err error
	hdr := req.Header()
	isNotification := hdr.MessageId == ^uint64(0)

	if _, ok := req.(*CancelRequest); !ok && !isNotification {
		creditCharge := hdr.CreditCharge

		conn.sequenceWindow += uint64(creditCharge)
		if hdr.CreditRequestResponse == 0 {
			hdr.CreditRequestResponse = creditCharge
		}

		hdr.CreditRequestResponse += conn.account.opening()
	}

	s := conn.session
	if _, ok := req.(*EchoResponse); ok && hdr.SessionId == 0 {
		s = nil
	}

	if s != nil && s.conn.useSession() {
		hdr.SessionId = s.sessionId

		if tc != nil {
			hdr.TreeId = tc.treeId
		}
	}

	size := req.Size()
	if hdr.NextCommand != 0 {
		size = Align(size, 8)
	}
	pkt := make([]byte, size)

	req.Encode(pkt)

	if s != nil {
		if _, ok := req.(*SessionSetupRequest); !ok {
			if s.sessionFlags&SMB2_SESSION_FLAG_ENCRYPT_DATA != 0 || (tc != nil && tc.shareFlags&SMB2_SHAREFLAG_ENCRYPT_DATA != 0) {
				pkt, err = s.encrypt(pkt)
				if err != nil {
					return nil, &InternalError{err.Error()}
				}
			} else {
				if s.sessionFlags&(SMB2_SESSION_FLAG_IS_GUEST|SMB2_SESSION_FLAG_IS_NULL) == 0 {
					pkt = s.sign(pkt)
				}
			}
		}
	}
	return pkt, nil
}

func (conn *conn) srvRecv() ([]byte, *session, *compoundContext, error) {
	return conn.outstandingRequests.pop(conn.ctx)
}

func (conn *conn) runSender() {
	for {
		select {
		case <-conn.wdone:
			log.Debugf("runsender finished")
			return
		case pkt := <-conn.write:
			_, err := conn.t.Write(pkt)

			select {
			case conn.werr <- err:
			case <-conn.ctx.Done():
				return
			}
		}
	}
}

func (conn *conn) runReciever() {
	var err error
	initialRequest := true

	for {
		n, e := conn.t.ReadSize()
		if e != nil {
			err = &TransportError{e}

			goto exit
		}

		pkt := make([]byte, n)

		_, e = conn.t.Read(pkt)
		if e != nil {
			err = &TransportError{e}

			goto exit
		}

		if len(pkt) < 4 {
			err = &InvalidRequestError{"short client packet header"}
			goto exit
		}
		if PacketCodec(pkt).IsSmb1() {
			// Only the initial SMB1 multiprotocol NEGOTIATE may reach the
			// existing SMB2 upgrade handler. SMB1 has no SMB2 compound fields;
			// its synthetic all-ones MessageId is not a client SMB2 bypass.
			if !initialRequest || len(pkt) < 35 || PacketCodec(pkt).Command() != SMB_COM_NEGOTIATE ||
				pkt[9]&0x80 != 0 || pkt[32] != 0 || int(binary.LittleEndian.Uint16(pkt[33:35])) != len(pkt)-35 {
				err = &InvalidRequestError{"invalid initial multiprotocol negotiate"}
				goto exit
			}
			initialRequest = false
			if e = conn.tryHandle(pkt, nil, nil, nil); e != nil {
				err = e
				goto exit
			}
			continue
		}
		initialRequest = false
		hasSession := conn.useSession()
		var encryptedSession *session
		var isEncrypted bool
		if hasSession {
			pkt, encryptedSession, e, isEncrypted = conn.tryDecrypt(pkt)
			if e != nil {
				err = e
				goto exit
			}
		}
		packets, e := splitRequests(pkt)
		if e != nil {
			err = e
			goto exit
		}
		var compCtx *compoundContext
		if len(packets) > 1 {
			first := PacketCodec(packets[0])
			compCtx = &compoundContext{treeId: uint64(first.TreeId()), sessionId: first.SessionId(), lastMsgId: PacketCodec(packets[len(packets)-1]).MessageId()}
		}
		var reqSession *session
		for _, part := range packets {
			p := PacketCodec(part)
			if hasSession {
				if p.Flags()&SMB2_FLAGS_RELATED_OPERATIONS == 0 {
					reqSession = conn.lookupSession(p.SessionId())
				}
				if encryptedSession != nil && reqSession != encryptedSession {
					err = &InvalidRequestError{"encrypted session mismatch"}
					goto exit
				}
				isSessionlessEcho := p.Command() == SMB2_ECHO && p.SessionId() == 0 && p.Flags()&SMB2_FLAGS_SIGNED == 0
				if reqSession == nil && p.Command() != SMB2_NEGOTIATE && p.Command() != SMB2_SESSION_SETUP && !isSessionlessEcho {
					err = &InvalidRequestError{"unknown session id"}
					goto exit
				}
				if e = conn.tryVerify(part, reqSession, isEncrypted); e != nil {
					err = e
					goto exit
				}
			}
			if e = conn.tryHandle(part, reqSession, compCtx, nil); e != nil {
				err = e
				goto exit
			}
		}
	}

exit:
	select {
	case <-conn.rdone:
		err = nil
	default:
		log.Errorln("error:", err)
	}

	conn.m.Lock()
	defer conn.m.Unlock()

	conn.outstandingRequests.shutdown(err)

	conn.err = err

	log.Debugf("receiver finished")

	conn.shutdown()
}

func accept(cmd uint16, pkt []byte) (res []byte, err error) {
	p := PacketCodec(pkt)
	if command := p.Command(); cmd != command {
		return nil, &InvalidResponseError{fmt.Sprintf("expected command: %v, got %v", cmd, command)}
	}

	// request, don't check status
	if (p.Flags() & SMB2_FLAGS_SERVER_TO_REDIR) == 0 {
		return p.Data(), nil
	}

	status := NtStatus(p.Status())

	switch status {
	case STATUS_SUCCESS:
		return p.Data(), nil
	case STATUS_OBJECT_NAME_COLLISION:
		return nil, os.ErrExist
	case STATUS_OBJECT_NAME_NOT_FOUND, STATUS_OBJECT_PATH_NOT_FOUND:
		return nil, os.ErrNotExist
	case STATUS_ACCESS_DENIED, STATUS_CANNOT_DELETE:
		return nil, os.ErrPermission
	}

	switch cmd {
	case SMB2_SESSION_SETUP:
		if status == STATUS_MORE_PROCESSING_REQUIRED {
			return p.Data(), nil
		}
	case SMB2_QUERY_INFO:
		if status == STATUS_BUFFER_OVERFLOW {
			return nil, &ResponseError{Code: uint32(status)}
		}
	case SMB2_IOCTL:
		if status == STATUS_BUFFER_OVERFLOW {
			if !IoctlResponseDecoder(p.Data()).IsInvalid() {
				return p.Data(), &ResponseError{Code: uint32(status)}
			}
		}
	case SMB2_READ:
		if status == STATUS_BUFFER_OVERFLOW {
			return nil, &ResponseError{Code: uint32(status)}
		}
	case SMB2_CHANGE_NOTIFY:
		if status == STATUS_NOTIFY_ENUM_DIR {
			return nil, &ResponseError{Code: uint32(status)}
		}
	}

	return nil, acceptError(uint32(status), p.Data())
}

func acceptError(status uint32, res []byte) error {
	r := ErrorResponseDecoder(res)
	if r.IsInvalid() {
		return &InvalidResponseError{"broken error response format"}
	}

	eData := r.ErrorData()

	if count := r.ErrorContextCount(); count != 0 {
		data := make([][]byte, count)
		for i := range data {
			ctx := ErrorContextResponseDecoder(eData)
			if ctx.IsInvalid() {
				return &InvalidResponseError{"broken error context response format"}
			}

			data[i] = ctx.ErrorContextData()

			next := ctx.Next()

			if len(eData) < next {
				return &InvalidResponseError{"broken error context response format"}
			}

			eData = eData[next:]
		}
		return &ResponseError{Code: status, data: data}
	}
	return &ResponseError{Code: status, data: [][]byte{eData}}
}

func (conn *conn) tryDecrypt(pkt []byte) ([]byte, *session, error, bool) {
	p := PacketCodec(pkt)
	if p.IsInvalid() {
		t := TransformCodec(pkt)
		if t.IsInvalid() {
			return nil, nil, &InvalidResponseError{"broken packet header format"}, false
		}

		if t.Flags() != Encrypted {
			return nil, nil, &InvalidResponseError{"encrypted flag is not on"}, false
		}

		s := conn.lookupSession(t.SessionId())
		if s == nil {
			return nil, nil, &InvalidResponseError{"unknown session id returned"}, false
		}

		pkt, err := s.decrypt(pkt)
		if err != nil {
			return nil, nil, &InvalidResponseError{err.Error()}, false
		}

		return pkt, s, nil, true
	}

	return pkt, nil, nil, false
}

func (conn *conn) tryVerify(pkt []byte, s *session, isEncrypted bool) error {
	p := PacketCodec(pkt)

	// These are client requests, not unsolicited server notifications. Even
	// a reserved MessageId cannot exempt a request from signature validation.
	// Related compound members may carry the all-ones inherited SessionId.
	// splitRequests rejects a related leading request; signatures still cover
	// the original bytes, not a rewritten effective-session header.
	matchesSession := s != nil && (s.sessionId == p.SessionId() || (p.Flags()&SMB2_FLAGS_RELATED_OPERATIONS != 0 && p.SessionId() == ^uint64(0)))
	if s != nil && !matchesSession {
		return &InvalidResponseError{"unknown session id returned"}
	}
	if p.Flags()&SMB2_FLAGS_SIGNED != 0 {
		if !matchesSession {
			return &InvalidResponseError{"unknown session id returned"}
		}
		if !s.verify(pkt) {
			return &InvalidResponseError{"unverified packet returned"}
		}
	} else if conn.requireSigning && !isEncrypted && s != nil &&
		s.sessionFlags&(SMB2_SESSION_FLAG_IS_GUEST|SMB2_SESSION_FLAG_IS_NULL) == 0 && matchesSession {
		return &InvalidResponseError{"signing required"}
	}

	return nil
}

func (conn *conn) tryHandle(pkt []byte, s *session, compCtx *compoundContext, e error) error {
	p := PacketCodec(pkt)

	msgId := p.MessageId()

	rr := &requestResponse{
		msgId:         msgId,
		creditRequest: p.CreditRequest(),
		pkt:           pkt,
		session:       s,
		ctx:           conn.ctx,
		recv:          make(chan []byte, 1),
		compCtx:       compCtx,
	}

	conn.outstandingRequests.set(msgId, rr)
	return nil
}

func (conn *conn) sendPacket(req Packet, tc *treeConn, compCtx *compoundContext) error {
	conn.m.Lock()

	if compCtx != nil {
		if !compCtx.isEmpty() {
			req.Header().Flags |= SMB2_FLAGS_RELATED_OPERATIONS
		}

		if req.Header().MessageId != compCtx.lastMsgId {
			l := Align(req.Size(), 8)
			req.Header().NextCommand = uint32(l)
		}
	}

	pkt, err := conn.encodePacket(req, tc, conn.ctx)
	if err != nil {
		conn.m.Unlock()
		return err
	}

	if compCtx != nil {
		compCtx.lastStatus = req.Header().Status

		compCtx.addResponse(pkt)
		if req.Header().MessageId != compCtx.lastMsgId {
			conn.m.Unlock()
			return nil
		}
		pkt = make([]byte, compCtx.Size())
		compCtx.Encode(pkt)
	}

	switch conn.serverState {
	case STATE_NEGOTIATE, STATE_SESSION_SETUP, STATE_SESSION_SETUP_CHALLENGE:
		conn.calcPreauthHash(pkt)
	}
	conn.m.Unlock()

	ctx := conn.ctx
	select {
	case conn.write <- pkt:
		select {
		case err = <-conn.werr:
			if err != nil {
				return &TransportError{err}
			}
		case <-ctx.Done():
			return &ContextError{Err: ctx.Err()}
		}
	case <-ctx.Done():
		return &ContextError{Err: ctx.Err()}
	}

	return nil
}
