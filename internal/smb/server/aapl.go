package server

import (
	"errors"
	"fmt"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// createAAPLContexts parses queries without changing connection state.
// Unknown requested bits and unrelated create contexts receive no grant.
func createAAPLContexts(contexts []wire.CreateContext) ([]wire.CreateContext, bool, error) {
	var replies []wire.CreateContext
	for _, context := range contexts {
		if context.Name != "AAPL" {
			continue
		}
		if len(replies) != 0 {
			return nil, false, errors.New("duplicate AAPL query")
		}
		query, err := wire.DecodeAAPLQuery(context)
		if err != nil {
			return nil, false, fmt.Errorf("decode AAPL query: %w", err)
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
			return nil, false, fmt.Errorf("encode AAPL reply: %w", err)
		}
		replies = append(replies, encoded)
	}
	return replies, len(replies) != 0, nil
}
