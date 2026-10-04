package smbtest_test

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func FuzzDecodeCreateReply(f *testing.F) {
	for _, contexts := range [][]wire.CreateContext{
		nil,
		{{Name: "RqLs", Data: make([]byte, 52)}, {Name: "DH2Q", Data: make([]byte, 8)}},
		{{Name: "RqLs", Data: []byte{1}}},
	} {
		body, err := wire.EncodeCreateResponse(wire.CreateResponse{ID: wire.FileID{Persistent: 1, Volatile: 2}, Contexts: contexts})
		if err != nil {
			f.Fatal(err)
		}
		f.Add(body)
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, body []byte) {
		_, err := smbtest.DecodeCreateReply(wire.Message{Header: wire.Header{Command: wire.Create, Flags: wire.FlagResponse}, Body: body})
		if err != nil {
			return
		}
	})
}
