package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestAAPLRequestedFields(t *testing.T) {
	for _, requested := range []uint64{0, 1, 2, 3, 4, 5, 6, 7, 0xffffffffffffffff} {
		t.Run(fmt.Sprintf("%x", requested), func(t *testing.T) {
			query, err := wire.EncodeAAPLQuery(wire.AAPLQuery{Requested: requested, ClientCapabilities: ^uint64(0)})
			if err != nil {
				t.Fatal(err)
			}
			contexts, validQuery, err := createAAPLContexts([]wire.CreateContext{{Name: "unknown"}, query})
			if err != nil {
				t.Fatal(err)
			}
			if !validQuery || len(contexts) != 1 {
				t.Fatalf("contexts = %v", contexts)
			}
			reply, err := wire.DecodeAAPLReply(contexts[0])
			if err != nil {
				t.Fatal(err)
			}
			want := wire.AAPLReply{Returned: requested & 7}
			if requested&2 != 0 {
				want.VolumeCapabilities = 0x06
			}
			if requested&4 != 0 {
				want.Model = "s3-smb"
			}
			if reply != want {
				t.Fatalf("reply = %+v, want %+v", reply, want)
			}
		})
	}
}

func TestAAPLInvalidContexts(t *testing.T) {
	query, err := wire.EncodeAAPLQuery(wire.AAPLQuery{Requested: 7})
	if err != nil {
		t.Fatal(err)
	}
	for _, contexts := range [][]wire.CreateContext{
		{{Name: "AAPL"}},
		{{Name: "AAPL", Data: append(append([]byte(nil), query.Data...), 0)}},
		{{Name: "AAPL", Data: append([]byte{2}, query.Data[1:]...)}},
		{query, query},
	} {
		if replies, validQuery, err := createAAPLContexts(contexts); err == nil || validQuery || len(replies) != 0 {
			t.Fatalf("accepted invalid contexts: %v", contexts)
		}
	}
	if contexts, validQuery, err := createAAPLContexts([]wire.CreateContext{{Name: "unknown"}}); err != nil || validQuery || len(contexts) != 0 {
		t.Fatalf("unknown context: %v, %v", contexts, err)
	}
}
