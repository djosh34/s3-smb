// Modified for s3-smb, 2026. See docs/vendored.md.

package smb2

import (
	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
)

// splitRequests validates the complete chain before dispatch, so response
// assembly knows its final MessageId before the first operation completes.
func splitRequests(pkt []byte) ([][]byte, error) {
	var packets [][]byte
	for {
		p := PacketCodec(pkt)
		if p.IsInvalid() || p.MessageId() == ^uint64(0) {
			return nil, &InvalidRequestError{"invalid client packet header or reserved message id"}
		}
		related := p.Flags()&SMB2_FLAGS_RELATED_OPERATIONS != 0
		if related && len(packets) == 0 {
			return nil, &InvalidRequestError{"related request without leading operation"}
		}
		if !related && p.SessionId() == ^uint64(0) {
			return nil, &InvalidRequestError{"inherited session without related operation"}
		}
		off := p.NextCommand()
		if off == 0 {
			return append(packets, pkt), nil
		}
		if off < 64 || off%8 != 0 || uint64(off)+64 > uint64(len(pkt)) {
			return nil, &InvalidRequestError{"invalid compound offset"}
		}
		packets = append(packets, pkt[:off])
		pkt = pkt[off:]
	}
}

type compoundContext struct {
	treeId     uint64
	sessionId  uint64
	fileId     *FileId
	lastStatus uint32
	lastMsgId  uint64

	rsp [][]byte
}

func (ctx *compoundContext) isEmpty() bool {
	return len(ctx.rsp) == 0
}

func (ctx *compoundContext) addResponse(pkt []byte) {
	ctx.rsp = append(ctx.rsp, pkt)
}

func (ctx *compoundContext) Size() int {
	s := 0
	for i, pkt := range ctx.rsp {
		if i != len(ctx.rsp)-1 {
			s += Align(len(pkt), 8)
		} else {
			s += len(pkt)
		}
	}
	return s
}

func (ctx *compoundContext) Encode(buf []byte) {
	off := 0
	for _, p := range ctx.rsp {
		pkt := PacketCodec(p)
		l := Align(len(p), 8)
		copy(buf[off:], pkt)
		off += l
	}
}
