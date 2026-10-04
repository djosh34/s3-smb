package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/crypt"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// reply is independent of header identity and credit allocation.
type reply struct {
	body   []byte
	status smb.Status
}

type handler func(context.Context, wire.Message) (reply, error)

type connection struct {
	conn        net.Conn
	closeErr    error
	ctx         context.Context
	preauth     *crypt.Preauth
	sender      *sender
	server      *Server
	cancel      context.CancelFunc
	pending     map[uint64]*pendingRequest
	credits     credits
	pendingMu   sync.Mutex
	workers     sync.WaitGroup
	closeOnce   sync.Once
	nextAsyncID uint64
	cipher      uint16
	signing     uint16
	clientGUID  [16]byte
	negotiated  bool
	opened      bool
}

func newConnection(ctx context.Context, cancel context.CancelFunc, server *Server, conn net.Conn) *connection {
	return &connection{ctx: ctx, cancel: cancel, server: server, conn: conn, sender: newSender(conn), preauth: crypt.NewPreauth(), credits: newCredits(), pending: make(map[uint64]*pendingRequest), nextAsyncID: 1}
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
		connection.server.options.Logger.Info("connection refused", "reason", err)
	}
	closeErr := connection.close()
	<-connection.sender.done
	connection.workers.Wait()
	return errors.Join(err, ctxErr, closeErr)
}

func readFrame(reader io.Reader) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, err
	}
	length := int(prefix[1])<<16 | int(prefix[2])<<8 | int(prefix[3])
	if prefix[0] != 0 || length < 32 || length > 0xffffff {
		return nil, errors.New("invalid direct TCP frame")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (connection *connection) receive(ctx context.Context) error {
	for {
		payload, err := readFrame(connection.conn)
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
		messages, err := wire.Split(payload)
		if err != nil {
			return err
		}
		if err := connection.checkNegotiationState(messages); err != nil {
			return err
		}
		if err := connection.process(ctx, messages); err != nil {
			return err
		}
	}
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
	payload, err := wire.Join(messages)
	if err != nil {
		return err
	}
	if len(messages) == 1 && messages[0].Header.Command == wire.Negotiate && messages[0].Header.Status == smb.StatusSuccess && connection.negotiated {
		connection.preauth.Update(payload)
	}
	return <-connection.sender.enqueue(payload)
}

func makeResponse(request wire.Header, result reply, credits uint16) (wire.Message, error) {
	header := wire.Header{
		MessageID: request.MessageID, SessionID: request.SessionID, ProcessID: request.ProcessID,
		TreeID: request.TreeID, Command: request.Command, CreditCharge: request.CreditCharge, Credit: credits, Flags: wire.FlagResponse | request.Flags&wire.FlagRelated, Status: result.status,
	}
	body := result.body
	if result.status != smb.StatusSuccess {
		var err error
		body, err = wire.EncodeErrorResponse(wire.ErrorResponse{})
		if err != nil {
			return wire.Message{}, err
		}
	}
	return wire.Message{Header: header, Body: body}, nil
}

func (connection *connection) dispatch(ctx context.Context, message wire.Message) (reply, error) {
	if message.Header.Command == wire.Negotiate {
		return connection.negotiate(message)
	}
	if handle, exists := connection.server.handlers[message.Header.Command]; exists {
		return handle(ctx, message)
	}
	if message.Header.Command <= wire.OplockBreak && (message.Header.Command != wire.SessionSetup || message.Header.SessionID != 0) {
		return reply{status: smb.StatusUserSessionDeleted}, nil
	}
	return reply{status: smb.StatusNotSupported}, nil
}

func handleEcho(_ context.Context, message wire.Message) (reply, error) {
	if message.Header.SessionID != 0 {
		return reply{status: smb.StatusUserSessionDeleted}, nil
	}
	body, err := wire.EncodeEchoResponse(wire.EmptyResponse{})
	return reply{body: body}, err
}
