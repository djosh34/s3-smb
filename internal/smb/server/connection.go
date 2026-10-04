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
	conn       net.Conn
	closeErr   error
	ctx        context.Context
	preauth    *crypt.Preauth
	sender     *sender
	server     *Server
	cancel     context.CancelFunc
	credits    credits
	closeOnce  sync.Once
	cipher     uint16
	signing    uint16
	clientGUID [16]byte
	negotiated bool
	wildcard   bool
}

func newConnection(ctx context.Context, server *Server, conn net.Conn) *connection {
	ctx, cancel := context.WithCancel(ctx)
	return &connection{ctx: ctx, cancel: cancel, server: server, conn: conn, sender: newSender(conn), preauth: crypt.NewPreauth(), credits: newCredits()}
}

func (connection *connection) close() error {
	connection.closeOnce.Do(func() {
		connection.cancel()
		connection.closeErr = connection.conn.Close()
	})
	return connection.closeErr
}

func (connection *connection) serve() error {
	stop := context.AfterFunc(connection.ctx, func() {
		if err := connection.close(); err != nil {
			connection.server.options.Logger.Error("close connection", "error", err)
		}
	})
	defer stop()
	go connection.sender.run(connection.ctx, connection.close)
	err := connection.receive()
	closeErr := connection.close()
	<-connection.sender.done
	return errors.Join(err, closeErr)
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

func (connection *connection) receive() error {
	for {
		payload, err := readFrame(connection.conn)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if bytes.HasPrefix(payload, []byte{0xff, 'S', 'M', 'B'}) {
			if connection.wildcard || connection.negotiated {
				return errors.New("SMB1 negotiate is not the opening request")
			}
			if decodeErr := wire.DecodeSMB1Negotiate(payload); decodeErr != nil {
				return decodeErr
			}
			if sendErr := connection.sendWildcard(); sendErr != nil {
				return sendErr
			}
			connection.wildcard = true
			continue
		}
		messages, err := wire.Split(payload)
		if err != nil {
			return err
		}
		if err := connection.process(messages); err != nil {
			return err
		}
	}
}

func (connection *connection) process(messages []wire.Message) error {
	validationErr := validateCompound(messages)
	if err := connection.credits.consume(messages); err != nil {
		return err
	}
	if validationErr != nil {
		connection.server.options.Logger.Info("compound refused", "reason", validationErr)
	}
	responses := make([]wire.Message, 0, len(messages))
	var preceding wire.Header
	for _, message := range messages {
		if message.Header.Flags&wire.FlagRelated != 0 {
			message.Header.SessionID, message.Header.TreeID = preceding.SessionID, preceding.TreeID
		}
		preceding = message.Header
		if message.Header.Command == wire.Cancel {
			continue
		}
		var result reply
		if validationErr != nil {
			result.status = smb.StatusInvalidParameter
		} else {
			var err error
			result, err = connection.dispatch(message)
			if err != nil {
				return err
			}
		}
		response, err := makeResponse(message.Header, result, connection.credits.grant(message.Header))
		if err != nil {
			return err
		}
		responses = append(responses, response)
	}
	if len(responses) == 0 {
		return nil
	}
	if len(responses) == 1 && responses[0].Header.Command == wire.Negotiate && responses[0].Header.Status == smb.StatusSuccess {
		payload, err := wire.Join(responses)
		if err != nil {
			return err
		}
		connection.preauth.Update(payload)
		return <-connection.sender.enqueue(payload)
	}
	return connection.send(responses)
}

func (connection *connection) send(messages []wire.Message) error {
	payload, err := wire.Join(messages)
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

func (connection *connection) dispatch(message wire.Message) (reply, error) {
	if message.Header.Command == wire.Negotiate {
		return connection.negotiate(message)
	}
	if handle, exists := connection.server.handlers[message.Header.Command]; exists {
		return handle(connection.ctx, message)
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
