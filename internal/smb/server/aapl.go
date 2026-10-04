package server

import (
	"errors"
	"fmt"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// createAAPLContexts answers each connection's query without negotiation state.
// Unknown requested bits and unrelated create contexts receive no grant.
func createAAPLContexts(request RequestContext, contexts []wire.CreateContext) ([]wire.CreateContext, error) {
	var replies []wire.CreateContext
	for _, context := range contexts {
		if context.Name != "AAPL" {
			continue
		}
		if len(replies) != 0 {
			return nil, errors.New("duplicate AAPL query")
		}
		query, err := wire.DecodeAAPLQuery(context)
		if err != nil {
			return nil, fmt.Errorf("decode AAPL query: %w", err)
		}
		reply := wire.AAPLReply{Returned: query.Requested & 7}
		if reply.Returned&1 != 0 {
			reply.ServerCapabilities = smb.AAPLServerCapabilities
		}
		if reply.Returned&2 != 0 {
			reply.VolumeCapabilities = smb.AAPLVolumeCapabilities
		}
		if reply.Returned&4 != 0 {
			reply.Model = "s3-smb"
		}
		encoded, err := wire.EncodeAAPLReply(reply)
		if err != nil {
			return nil, fmt.Errorf("encode AAPL reply: %w", err)
		}
		replies = append(replies, encoded)
	}
	if len(replies) != 0 {
		request.markAAPL()
	}
	return replies, nil
}
