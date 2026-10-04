package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// reply is independent of header identity and credit allocation.
type reply struct {
	body      []byte
	fileID    wire.FileID
	sessionID uint64
	treeID    uint32
	status    smb.Status
}

type handler func(context.Context, RequestContext, wire.Message) (reply, error)

type connection struct {
	conn            net.Conn
	closeErr        error
	ctx             context.Context
	preauth         *crypt.Preauth
	sender          *sender
	server          *Server
	cancel          context.CancelFunc
	pending         map[uint64]*pendingRequest
	sessions        map[uint64]*sessionEntry
	replyProtection map[uint64]savedProtection
	inflight        map[*sessionRequest]struct{}
	credits         credits
	sessionMu       sync.RWMutex
	pendingMu       sync.Mutex
	workers         sync.WaitGroup
	closeOnce       sync.Once
	aapl            atomic.Bool
	nextAsyncID     uint64
	cipher          uint16
	signing         uint16
	clientGUID      [16]byte
	negotiated      bool
	opened          bool
}

func newConnection(ctx context.Context, cancel context.CancelFunc, server *Server, conn net.Conn) *connection {
	return &connection{ctx: ctx, cancel: cancel, server: server, conn: conn, sender: newSender(conn), preauth: crypt.NewPreauth(), credits: newCredits(), pending: make(map[uint64]*pendingRequest), sessions: make(map[uint64]*sessionEntry), replyProtection: make(map[uint64]savedProtection), nextAsyncID: 1}
}

func (connection *connection) close() error {
	connection.closeOnce.Do(func() {
		connection.cancel()
		connection.closeErr = connection.conn.Close()
	})
	return connection.closeErr
}

func (connection *connection) serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() {
		if err := connection.close(); err != nil {
			connection.server.options.Logger.Error("close connection", "error", err)
		}
	})
	defer stop()
	go connection.sender.run(ctx, connection.close)
	err := connection.receive(ctx)
	ctxErr := ctx.Err()
	if err != nil {
		connection.server.options.Logger.Info("connection closed", "reason", err)
	}
	closeErr := connection.close()
	actions := connection.detachSessions()
	<-connection.sender.done
	connection.workers.Wait()
	cleanupErr := connection.server.cleanup(context.WithoutCancel(ctx), actions)
	return errors.Join(err, ctxErr, closeErr, cleanupErr)
}

func readFrame(reader io.Reader, maxLength uint32) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, err
	}
	length := uint32(prefix[1])<<16 | uint32(prefix[2])<<8 | uint32(prefix[3])
	if prefix[0] != 0 || length < 32 {
		return nil, errors.New("invalid direct TCP frame")
	}
	if length > maxLength {
		return nil, errors.New("direct TCP frame exceeds connection limit")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (connection *connection) receive(ctx context.Context) error {
	for {
		payload, err := readFrame(connection.conn, connection.frameLimit())
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if bytes.HasPrefix(payload, []byte{0xff, 'S', 'M', 'B'}) {
			if connection.opened {
				return errors.New("SMB1 negotiate is not the opening request")
			}
			if decodeErr := wire.DecodeSMB1Negotiate(payload); decodeErr != nil {
				return decodeErr
			}
			if sendErr := connection.sendWildcard(); sendErr != nil {
				return sendErr
			}
			connection.opened = true
			continue
		}
		connection.opened = true
		messages, err := connection.decodePayload(payload)
		if err != nil && !errors.Is(err, errAccessDenied) {
			return err
		}
		denied := errors.Is(err, errAccessDenied)
		if err := connection.checkNegotiationState(messages); err != nil {
			return err
		}
		if err := connection.process(ctx, messages, denied); err != nil {
			return err
		}
	}
}

func (connection *connection) frameLimit() uint32 {
	if !connection.negotiated {
		return smb.CreditUnit
	}
	// Allow 64 KiB for headers, fixed bodies and compound padding, in addition
	// to the largest transfer size advertised in the NEGOTIATE response.
	return max(smb.MaxTransactSize, smb.MaxReadSize, smb.MaxWriteSize) + smb.CreditUnit
}

func (connection *connection) checkNegotiationState(messages []wire.Message) error {
	if !connection.negotiated && (len(messages) != 1 || messages[0].Header.Command != wire.Negotiate) {
		return errors.New("request before NEGOTIATE")
	}
	if connection.negotiated {
		for _, message := range messages {
			if message.Header.Command == wire.Negotiate {
				return errors.New("connection already negotiated")
			}
		}
	}
	return nil
}

func (connection *connection) send(messages []wire.Message) error {
	if len(messages) == 0 {
		return nil
	}
	if connection.mixedEncryption(messages) {
		// One transform belongs to one session. Policy-error replies to a
		// plaintext compound spanning encrypted sessions need separate frames.
		for _, message := range messages {
			if err := connection.sendFrame([]wire.Message{message}); err != nil {
				return err
			}
		}
		return nil
	}
	return connection.sendFrame(messages)
}

func (connection *connection) sendFrame(messages []wire.Message) error {
	payload, err := connection.encodePayload(messages)
	if err != nil {
		return err
	}
	return <-connection.sender.enqueue(payload)
}

func makeResponse(request wire.Header, result reply, credits uint16) (wire.Message, error) {
	header := wire.Header{
		MessageID: request.MessageID, SessionID: request.SessionID, ProcessID: request.ProcessID,
		TreeID: request.TreeID, Command: request.Command, CreditCharge: request.CreditCharge, Credit: credits, Flags: wire.FlagResponse | request.Flags&wire.FlagRelated, Status: result.status,
	}
	if result.sessionID != 0 {
		header.SessionID = result.sessionID
	}
	if result.treeID != 0 {
		header.TreeID = result.treeID
	}
	body := result.body
	if errorBodyRequired(request.Command, result.status) {
		var err error
		body, err = wire.EncodeErrorResponse(wire.ErrorResponse{})
		if err != nil {
			return wire.Message{}, err
		}
	}
	return wire.Message{Header: header, Body: body}, nil
}

// These command statuses carry a normal response body (MS-SMB2 3.3.4.4).
func errorBodyRequired(command wire.Command, status smb.Status) bool {
	if status == smb.StatusSuccess {
		return false
	}
	if status == smb.StatusMoreProcessingRequired && command == wire.SessionSetup {
		return false
	}
	if status == smb.StatusBufferOverflow && (command == wire.QueryInfo || command == wire.IOCTL || command == wire.Read) {
		return false
	}
	return true
}

func (connection *connection) dispatch(ctx context.Context, message wire.Message, previous compoundState) (reply, error) {
	if message.Header.Command == wire.Negotiate {
		return connection.negotiate(message)
	}
	if message.Header.Command == wire.SessionSetup {
		return connection.sessionSetup(ctx, message)
	}
	if message.Header.Command > wire.OplockBreak {
		return reply{status: smb.StatusNotSupported}, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, operation, status := connection.resolveRequest(message.Header, cancel)
	if status != smb.StatusSuccess {
		return reply{status: status}, nil
	}
	defer connection.finishRequest(operation)
	switch uint16(message.Header.Command) {
	case uint16(wire.ChangeNotify):
		return reply{status: smb.StatusNotSupported}, nil
	case uint16(wire.TreeConnect):
		return connection.treeConnect(message)
	case uint16(wire.Logoff):
		return connection.logoff(ctx, message)
	case uint16(wire.TreeDisconnect):
		return connection.treeDisconnect(ctx, message)
	}
	if handle, exists := connection.server.handlers[message.Header.Command]; exists {
		request.related = message.Header.Flags&wire.FlagRelated != 0
		request.fileID = previous.fileID
		return handle(ctx, request, message)
	}
	return reply{status: smb.StatusNotSupported}, nil
}

func handleEcho(_ context.Context, _ RequestContext, _ wire.Message) (reply, error) {
	body, err := wire.EncodeEchoResponse(wire.EmptyResponse{})
	return reply{body: body}, err
}
