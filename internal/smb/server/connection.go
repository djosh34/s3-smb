package server

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// reply is independent of header identity and credit allocation.
type reply struct {
	body   []byte
	status smb.Status
}

type handler func(context.Context, wire.Message) (reply, error)

type connection struct {
	ctx       context.Context
	cancel    context.CancelFunc
	server    *Server
	conn      net.Conn
	sender    *sender
	closeErr  error
	closeOnce sync.Once
}

func newConnection(ctx context.Context, server *Server, conn net.Conn) *connection {
	ctx, cancel := context.WithCancel(ctx)
	return &connection{ctx: ctx, cancel: cancel, server: server, conn: conn, sender: newSender(conn)}
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
	responses := make([]wire.Message, 0, len(messages))
	for _, message := range messages {
		result := reply{status: smb.StatusNotSupported}
		if handle, exists := connection.server.handlers[message.Header.Command]; exists {
			var err error
			result, err = handle(connection.ctx, message)
			if err != nil {
				result = reply{status: smb.StatusInvalidParameter}
			}
		}
		response, err := makeResponse(message.Header, result, 1)
		if err != nil {
			return err
		}
		responses = append(responses, response)
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
	header := wire.Header{MessageID: request.MessageID, SessionID: request.SessionID, ProcessID: request.ProcessID,
		TreeID: request.TreeID, Command: request.Command, CreditCharge: request.CreditCharge, Credit: credits, Flags: wire.FlagResponse, Status: result.status}
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

func handleEcho(_ context.Context, message wire.Message) (reply, error) {
	if _, err := wire.DecodeEchoRequest(message); err != nil {
		return reply{}, err
	}
	body, err := wire.EncodeEchoResponse(wire.EmptyResponse{})
	return reply{body: body}, err
}
