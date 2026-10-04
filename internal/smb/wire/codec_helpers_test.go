package wire

import (
	"bytes"
	"reflect"
	"testing"
)

func checkRoundTrip[T any](t *testing.T, value T, encode func(T) ([]byte, error), decode func([]byte) (T, error)) {
	t.Helper()
	data, err := encode(value)
	if err != nil {
		t.Fatal(err)
	}
	saved := clone(data)
	got, err := decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, value) {
		t.Fatalf("round trip: got %+v, want %+v", got, value)
	}
	again, err := encode(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Fatal("encoding changed after round trip")
	}
	for i := range data {
		data[i] ^= 0xff
	}
	afterMutation, err := encode(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, afterMutation) {
		t.Fatal("decoded value aliases input")
	}
	var zero T
	if empty, encodeErr := encode(zero); encodeErr == nil {
		decoded, decodeErr := decode(empty)
		if decodeErr != nil {
			t.Fatalf("zero value failed to decode: %v", decodeErr)
		}
		if !reflect.DeepEqual(decoded, zero) {
			t.Fatalf("zero value changed: %+v", decoded)
		}
	}
}

func checkMessageEnvelope[T any](t *testing.T, value T, encode func(T) ([]byte, error), decode func(Message) (T, error), command Command, response bool) {
	t.Helper()
	data, err := encode(value)
	if err != nil {
		t.Fatal(err)
	}
	flags := HeaderFlags(0)
	if response {
		flags = FlagResponse
	}
	m := Message{Header: Header{Command: command, Flags: flags}, Body: data}
	m.Header.Command = Command(0xffff)
	// ErrorResponse is deliberately command-independent.
	if reflect.TypeOf(value) != reflect.TypeOf(ErrorResponse{}) {
		if _, err := decode(m); err == nil {
			t.Fatal("accepted wrong command")
		}
	}
	m.Header.Command = command
	m.Header.Flags ^= FlagResponse
	if _, err := decode(m); err == nil {
		t.Fatal("accepted wrong direction")
	}
	m.Header.Flags = flags
	for n := 0; n < 2; n++ {
		m.Body = data[:n]
		if _, err := decode(m); err == nil {
			t.Fatal("accepted short structure size")
		}
	}
	m.Body = clone(data)
	m.Body[0] = 0
	m.Body[1] = 0
	if _, err := decode(m); err == nil {
		t.Fatal("accepted wrong structure size")
	}
}

func fuzzCodec[T any](f *testing.F, value T, encode func(T) ([]byte, error), decode func([]byte) (T, error)) {
	f.Helper()
	seed, err := encode(value)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add(seed[:len(seed)/2])
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		value, err := decode(data)
		if err != nil {
			return
		}
		encoded, err := encode(value)
		if err != nil {
			t.Fatalf("accepted value cannot encode: %v", err)
		}
		again, err := decode(encoded)
		if err != nil {
			t.Fatalf("encoded value cannot decode: %v", err)
		}
		canonical, err := encode(again)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(encoded, canonical) {
			t.Fatal("unstable canonical encoding")
		}
	})
}
