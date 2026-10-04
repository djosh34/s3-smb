package wire

import "testing"

func TestTerminalListPadding(t *testing.T) {
	stream, err := EncodeFileStreamInformation(FileStreamInformation{Entries: []FileStreamEntry{{Name: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	both, err := EncodeDirectoryIDBothEntries([]DirectoryIDBothEntry{{Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	full, err := EncodeDirectoryIDFullEntries([]DirectoryIDFullEntry{{Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	create, err := encodeCreateContexts([]CreateContext{{Name: "test", Data: []byte{1}}})
	if err != nil {
		t.Fatal(err)
	}
	negotiate, err := encodeNegotiateContexts([]NegotiateContext{{Type: 0xffff, Data: []byte{1}}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		decode func([]byte) error
		data   []byte
	}{
		{data: stream, decode: func(b []byte) error { _, err := DecodeFileStreamInformation(b); return err }},
		{data: both, decode: func(b []byte) error { _, err := DecodeDirectoryIDBothEntries(b); return err }},
		{data: full, decode: func(b []byte) error { _, err := DecodeDirectoryIDFullEntries(b); return err }},
		{data: create, decode: func(b []byte) error { _, err := decodeCreateContexts(b); return err }},
		{data: negotiate, decode: func(b []byte) error { _, err := decodeNegotiateContexts(b, 1); return err }},
	}
	for _, c := range cases {
		for padding := 0; padding <= 8; padding++ {
			b := append(clone(c.data), make([]byte, padding)...)
			decodeErr := c.decode(b)
			if padding <= 7 && decodeErr != nil {
				t.Fatalf("rejected %d padding bytes: %v", padding, decodeErr)
			}
			if padding == 8 && decodeErr == nil {
				t.Fatal("accepted data after the terminal member")
			}
		}
	}
}
