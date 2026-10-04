package wire

import (
	"reflect"
	"testing"
)

func TestOplockBreakRequest(t *testing.T) {
	want := OplockBreakRequest{ID: FileID{Persistent: 7, Volatile: 8}, Level: 1}
	encoded, err := EncodeOplockBreakRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeOplockBreakRequest(Message{Header: Header{Command: OplockBreak}, Body: encoded})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	for length := 0; length < len(encoded); length++ {
		if _, err := DecodeOplockBreakRequest(Message{Header: Header{Command: OplockBreak}, Body: encoded[:length]}); err == nil {
			t.Fatalf("accepted short body length %d", length)
		}
	}
	for _, header := range []Header{{Command: Echo}, {Command: OplockBreak, Flags: FlagResponse}} {
		if _, err := DecodeOplockBreakRequest(Message{Header: header, Body: encoded}); err == nil {
			t.Fatal("accepted wrong command or response")
		}
	}
}

func FuzzDecodeOplockBreakRequest(f *testing.F) {
	fuzzCodec(f, OplockBreakRequest{ID: FileID{Persistent: 7, Volatile: 8}, Level: 1}, EncodeOplockBreakRequest, func(data []byte) (OplockBreakRequest, error) {
		return DecodeOplockBreakRequest(Message{Header: Header{Command: OplockBreak}, Body: data})
	})
}
