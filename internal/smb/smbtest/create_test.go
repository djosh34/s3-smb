package smbtest_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCreateContextsAndReplay(t *testing.T) {
	lease := wire.LeaseContext{Version: 2, Key: [16]byte{1, 2}, ParentKey: [16]byte{3, 4}, State: 7, Flags: 4, Epoch: 9, Duration: 12}
	durable := wire.DurableRequest{CreateGUID: [16]byte{5, 6}, Timeout: 960000, Flags: 2}
	reconnect := wire.DurableReconnect{ID: wire.FileID{Persistent: 17, Volatile: 18}, CreateGUID: durable.CreateGUID, Flags: 2}
	for _, reconnecting := range []bool{false, true} {
		for _, replay := range []bool{false, true} {
			t.Run(fmt.Sprintf("reconnect_%t_replay_%t", reconnecting, replay), func(t *testing.T) {
				request := wire.CreateRequest{Name: "band", DesiredAccess: 0x12019f, ShareAccess: 7, Disposition: 1, Options: 0x40, FileAttributes: 0x80, ImpersonationLevel: 2, Contexts: []wire.CreateContext{{Name: "unknown", Data: []byte{1, 2}}}}
				options := smbtest.CreateOptions{Request: request, Lease: &lease, Replay: replay}
				if reconnecting {
					options.Reconnect = &reconnect
				} else {
					options.Durable = &durable
				}
				header := wire.Header{MessageID: 9, SessionID: 42, TreeID: 3, Credit: 8, CreditCharge: 1}
				client := fakePeer(t, func(peer net.Conn) error {
					payload, err := readFrame(peer)
					if err != nil {
						return err
					}
					return checkCreateBytes(payload, header, options)
				})
				if err := client.SendCreate(t.Context(), header, options); err != nil {
					t.Fatal(err)
				}
				if request.OplockLevel != 0 || len(request.Contexts) != 1 || lease.Version != 2 {
					t.Fatal("SendCreate changed caller fields")
				}
			})
		}
	}
}

func checkCreateBytes(payload []byte, header wire.Header, options smbtest.CreateOptions) error {
	messages, err := wire.Split(payload)
	if err != nil {
		return err
	}
	header.Command = wire.Create
	if options.Replay {
		header.Flags |= wire.FlagReplay
	}
	if len(messages) != 1 || messages[0].Header != header {
		return errors.New("CREATE header changed")
	}
	request, err := wire.DecodeCreateRequest(messages[0])
	if err != nil {
		return err
	}
	contexts := request.Contexts
	request.Contexts = options.Request.Contexts
	want := options.Request
	want.OplockLevel = 0xff
	if !reflect.DeepEqual(request, want) || len(contexts) != len(want.Contexts)+2 {
		return errors.New("CREATE fields changed")
	}
	leaseBytes := make([]byte, 52)
	copy(leaseBytes, options.Lease.Key[:])
	binary.LittleEndian.PutUint32(leaseBytes[16:], options.Lease.State)
	binary.LittleEndian.PutUint32(leaseBytes[20:], options.Lease.Flags)
	binary.LittleEndian.PutUint64(leaseBytes[24:], options.Lease.Duration)
	copy(leaseBytes[32:], options.Lease.ParentKey[:])
	binary.LittleEndian.PutUint16(leaseBytes[48:], options.Lease.Epoch)
	leaseContext := contexts[len(contexts)-2]
	if leaseContext.Name != "RqLs" || !bytes.Equal(leaseContext.Data, leaseBytes) {
		return errors.New("RqLs V2 bytes changed")
	}
	last := contexts[len(contexts)-1]
	if options.Durable != nil {
		data := make([]byte, 32)
		binary.LittleEndian.PutUint32(data, options.Durable.Timeout)
		binary.LittleEndian.PutUint32(data[4:], options.Durable.Flags)
		copy(data[16:], options.Durable.CreateGUID[:])
		if last.Name != "DH2Q" || !bytes.Equal(last.Data, data) {
			return errors.New("DH2Q bytes changed")
		}
	} else {
		data := make([]byte, 36)
		binary.LittleEndian.PutUint64(data, options.Reconnect.ID.Persistent)
		binary.LittleEndian.PutUint64(data[8:], options.Reconnect.ID.Volatile)
		copy(data[16:], options.Reconnect.CreateGUID[:])
		binary.LittleEndian.PutUint32(data[32:], options.Reconnect.Flags)
		if last.Name != "DH2C" || !bytes.Equal(last.Data, data) {
			return errors.New("DH2C bytes changed")
		}
	}
	return nil
}

func createReply(t *testing.T, contexts []wire.CreateContext) wire.Message {
	t.Helper()
	body, err := wire.EncodeCreateResponse(wire.CreateResponse{ID: wire.FileID{Persistent: 2, Volatile: 3}, OplockLevel: 0xff, Action: 1, Contexts: contexts})
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.Create, Flags: wire.FlagResponse}, Body: body}
}

func TestDecodeCreateGrants(t *testing.T) {
	lease := wire.LeaseContext{Version: 2, Key: [16]byte{1}, State: 3, Epoch: 19}
	durable := wire.DurableReply{Timeout: 120000}
	leaseContext, err := wire.EncodeLeaseContext(lease)
	if err != nil {
		t.Fatal(err)
	}
	durableContext, err := wire.EncodeDurableReply(durable)
	if err != nil {
		t.Fatal(err)
	}
	contexts := []wire.CreateContext{leaseContext, durableContext, {Name: "unknown", Data: []byte{1, 2}}}
	message := createReply(t, contexts)
	client := fakePeer(t, func(peer net.Conn) error { return writeMessages(peer, message) })
	reply, err := client.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, err := smbtest.DecodeCreateReply(reply.Messages[0])
	if err != nil || result.Lease == nil || *result.Lease != lease || result.Durable == nil || *result.Durable != durable || !reflect.DeepEqual(result.Reply.Contexts, contexts) {
		t.Fatalf("CREATE result = %+v, error = %v", result, err)
	}
	result, err = smbtest.DecodeCreateReply(createReply(t, nil))
	if err != nil || result.Lease != nil || result.Durable != nil {
		t.Fatalf("no-grant CREATE = %+v, error = %v", result, err)
	}
	for _, invalid := range [][]wire.CreateContext{
		{leaseContext, leaseContext},
		{durableContext, durableContext},
		{{Name: "RqLs", Data: make([]byte, 32)}},
		{{Name: "RqLs", Data: []byte{1}}},
		{{Name: "DH2Q", Data: []byte{1}}},
	} {
		if _, err := smbtest.DecodeCreateReply(createReply(t, invalid)); err == nil {
			t.Fatal("invalid grant accepted")
		}
	}
	for _, change := range []func(*wire.Message){
		func(m *wire.Message) { m.Header.Command = wire.Echo },
		func(m *wire.Message) { m.Header.Flags = 0 },
		func(m *wire.Message) { m.Header.Status = smb.StatusAccessDenied },
		func(m *wire.Message) { m.Body = m.Body[:3] },
	} {
		invalid := message
		change(&invalid)
		if _, err := smbtest.DecodeCreateReply(invalid); err == nil {
			t.Fatal("invalid CREATE reply accepted")
		}
	}
}

func TestInvalidCreateOptionsDoNotWrite(t *testing.T) {
	client := fakePeer(t, func(peer net.Conn) error {
		_, err := readFrame(peer)
		return err
	})
	for _, options := range []smbtest.CreateOptions{
		{Lease: &wire.LeaseContext{Version: 1}},
		{Durable: &wire.DurableRequest{}, Reconnect: &wire.DurableReconnect{}},
		{Request: wire.CreateRequest{Contexts: []wire.CreateContext{{Name: "RqLs"}}}},
		{Request: wire.CreateRequest{Contexts: []wire.CreateContext{{Name: "DH2Q"}}}},
		{Request: wire.CreateRequest{Contexts: []wire.CreateContext{{Name: "DH2C"}}}},
	} {
		if err := client.SendCreate(t.Context(), wire.Header{}, options); err == nil {
			t.Fatal("invalid CREATE options accepted")
		}
	}
	if err := client.SendCreate(t.Context(), wire.Header{}, smbtest.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}
