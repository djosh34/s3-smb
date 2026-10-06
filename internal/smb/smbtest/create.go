package smbtest

import (
	"context"
	"errors"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// CreateOptions adds typed lease and durable contexts to Request. Its other
// contexts and fields are preserved. Lease must be V2 and selects oplock level
// lease. Durable and Reconnect are mutually exclusive. Replay sets FlagReplay.
// Use Send with wire codecs or SendRaw to send deliberately invalid contexts.
type CreateOptions struct {
	Lease     *wire.LeaseContext
	Durable   *wire.DurableRequest
	Reconnect *wire.DurableReconnect
	Request   wire.CreateRequest
	Replay    bool
}

// CreateResult retains all reply fields and contexts, with typed lease and
// durable grants when present. A missing context means no grant, not an error.
type CreateResult struct {
	Lease   *wire.LeaseContext
	Durable *wire.DurableReply
	Reply   wire.CreateResponse
}

// SendCreate sends CREATE with supplied identities and credits. It does not
// allocate message IDs or wait for a reply. Receive and DecodeCreateReply read
// the result, so tests can handle a lease break while CREATE is in flight.
func (client *Client) SendCreate(ctx context.Context, header wire.Header, options CreateOptions) error {
	message, err := CreateMessage(header, options)
	if err != nil {
		return err
	}
	return client.Send(ctx, []wire.Message{message})
}

// CreateMessage builds the CREATE request that SendCreate sends, for a
// compound.
func CreateMessage(header wire.Header, options CreateOptions) (wire.Message, error) {
	request, err := createRequest(options)
	if err != nil {
		return wire.Message{}, err
	}
	body, err := wire.EncodeCreateRequest(request)
	if err != nil {
		return wire.Message{}, err
	}
	header.Command = wire.Create
	if options.Replay {
		header.Flags |= wire.FlagReplay
	}
	return wire.Message{Header: header, Body: body}, nil
}

func createRequest(options CreateOptions) (wire.CreateRequest, error) {
	request := options.Request
	request.Contexts = append([]wire.CreateContext(nil), request.Contexts...)
	if options.Durable != nil && options.Reconnect != nil {
		return wire.CreateRequest{}, errors.New("smbtest: DH2Q and DH2C are mutually exclusive")
	}
	for _, context := range request.Contexts {
		if context.Name == "RqLs" || context.Name == "DH2Q" || context.Name == "DH2C" {
			return wire.CreateRequest{}, errors.New("smbtest: use typed lease and durable contexts")
		}
	}
	if options.Lease != nil {
		if options.Lease.Version != 2 {
			return wire.CreateRequest{}, errors.New("smbtest: lease must be V2")
		}
		context, err := wire.EncodeLeaseContext(*options.Lease)
		if err != nil {
			return wire.CreateRequest{}, err
		}
		request.Contexts = append(request.Contexts, context)
		request.OplockLevel = 0xff
	}
	if options.Durable != nil {
		context, err := wire.EncodeDurableRequest(*options.Durable)
		if err != nil {
			return wire.CreateRequest{}, err
		}
		request.Contexts = append(request.Contexts, context)
	}
	if options.Reconnect != nil {
		context, err := wire.EncodeDurableReconnect(*options.Reconnect)
		if err != nil {
			return wire.CreateRequest{}, err
		}
		request.Contexts = append(request.Contexts, context)
	}
	return request, nil
}

// DecodeCreateReply checks the status and decodes RqLs V2 and DH2Q grants.
// Unknown contexts remain in Reply. Duplicate grants and malformed contexts fail.
func DecodeCreateReply(message wire.Message) (CreateResult, error) {
	if message.Header.Command != wire.Create || message.Header.Flags&wire.FlagResponse == 0 {
		return CreateResult{}, errors.New("smbtest: not a CREATE reply")
	}
	if message.Header.Status != smb.StatusSuccess {
		return CreateResult{}, loginStatus(message)
	}
	reply, err := wire.DecodeCreateResponse(message)
	if err != nil {
		return CreateResult{}, err
	}
	result := CreateResult{Reply: reply}
	for _, context := range reply.Contexts {
		switch context.Name {
		case "RqLs":
			if result.Lease != nil {
				return CreateResult{}, errors.New("smbtest: duplicate lease grant")
			}
			lease, err := wire.DecodeLeaseContext(context)
			if err != nil {
				return CreateResult{}, err
			}
			if lease.Version != 2 {
				return CreateResult{}, errors.New("smbtest: lease grant must be V2")
			}
			result.Lease = &lease
		case "DH2Q":
			if result.Durable != nil {
				return CreateResult{}, errors.New("smbtest: duplicate durable grant")
			}
			durable, err := wire.DecodeDurableReply(context)
			if err != nil {
				return CreateResult{}, err
			}
			result.Durable = &durable
		}
	}
	return result, nil
}
