package smbtest_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func breakMessage(t *testing.T) (wire.Message, wire.LeaseBreakNotification) {
	t.Helper()
	notification := wire.LeaseBreakNotification{Key: [16]byte{1, 2, 3}, Epoch: 9, Flags: 1, CurrentState: 7, NewState: 3}
	body, err := wire.EncodeLeaseBreakNotification(notification)
	if err != nil {
		t.Fatal(err)
	}
	return wire.Message{Header: wire.Header{Command: wire.OplockBreak, MessageID: ^uint64(0), Flags: wire.FlagResponse}, Body: body}, notification
}

func TestLeaseBreakRouting(t *testing.T) {
	for _, waitFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "receive first", true: "wait first"}[waitFirst], func(t *testing.T) {
			checkLeaseBreakRouting(t, waitFirst)
		})
	}
}

func checkLeaseBreakRouting(t *testing.T, waitFirst bool) {
	t.Helper()
	notification, want := breakMessage(t)
	client := fakePeer(t, func(peer net.Conn) error {
		if err := writeMessages(peer, response(10, 110, smb.StatusPending, 5)); err != nil {
			return err
		}
		if err := writeMessages(peer, notification, response(10, 110, smb.StatusSuccess, 0)); err != nil {
			return err
		}
		return writeMessages(peer, notification)
	})
	if waitFirst {
		got, err := client.WaitLeaseBreak(t.Context())
		if err != nil || got != want {
			t.Fatalf("break = %+v, error = %v", got, err)
		}
	}
	for _, status := range []smb.Status{smb.StatusPending, smb.StatusSuccess} {
		reply, err := client.Receive(t.Context())
		if err != nil || len(reply.Messages) != 1 || reply.Messages[0].Header.Status != status || reply.Messages[0].Header.MessageID != 10 {
			t.Fatalf("reply = %+v, error = %v", reply, err)
		}
	}
	count := 2
	if waitFirst {
		count--
	}
	for range count {
		got, err := client.WaitLeaseBreak(t.Context())
		if err != nil || got != want {
			t.Fatalf("break = %+v, error = %v", got, err)
		}
	}
}

func TestReceiveSkipsStandaloneLeaseBreak(t *testing.T) {
	notification, want := breakMessage(t)
	client := fakePeer(t, func(peer net.Conn) error {
		if err := writeMessages(peer, notification); err != nil {
			return err
		}
		return writeMessages(peer, response(2, 0, smb.StatusSuccess, 1))
	})
	reply, err := client.Receive(t.Context())
	if err != nil || len(reply.Messages) != 1 || reply.Messages[0].Header.MessageID != 2 {
		t.Fatalf("reply = %+v, error = %v", reply, err)
	}
	got, err := client.WaitLeaseBreak(t.Context())
	if err != nil || got != want {
		t.Fatalf("queued break = %+v, error = %v", got, err)
	}
}

func TestLeaseBreakRejectsMalformedNotifications(t *testing.T) {
	for _, change := range []func(*wire.Message){
		func(m *wire.Message) { m.Header.SessionID = 42 },
		func(m *wire.Message) { m.Header.Command = wire.Echo },
		func(m *wire.Message) { m.Header.Flags = 0 },
		func(m *wire.Message) { m.Header.Flags |= wire.FlagAsync },
		func(m *wire.Message) { m.Header.Status = smb.StatusAccessDenied },
		func(m *wire.Message) { m.Body = m.Body[:3] },
	} {
		message, _ := breakMessage(t)
		change(&message)
		client := fakePeer(t, func(peer net.Conn) error { return writeMessages(peer, message) })
		if _, err := client.WaitLeaseBreak(t.Context()); err == nil {
			t.Fatal("invalid notification accepted")
		}
	}
}

func TestSendLeaseBreakAcknowledgment(t *testing.T) {
	header := wire.Header{MessageID: 18, SessionID: 42, TreeID: 3, CreditCharge: 1, Credit: 7}
	ack := wire.LeaseBreakRequest{Key: [16]byte{9, 8, 7}, State: 3, Flags: 2, Duration: 4}
	client := fakePeer(t, func(peer net.Conn) error {
		payload, err := readFrame(peer)
		if err != nil {
			return err
		}
		messages, err := wire.Split(payload)
		if err != nil {
			return err
		}
		wantHeader := header
		wantHeader.Command = wire.OplockBreak
		wantBody := make([]byte, 36)
		binary.LittleEndian.PutUint16(wantBody, 36)
		binary.LittleEndian.PutUint32(wantBody[4:], ack.Flags)
		copy(wantBody[8:24], ack.Key[:])
		binary.LittleEndian.PutUint32(wantBody[24:], ack.State)
		binary.LittleEndian.PutUint64(wantBody[28:], ack.Duration)
		if len(messages) != 1 || messages[0].Header != wantHeader || !bytes.Equal(messages[0].Body, wantBody) {
			return errors.New("acknowledgment bytes or identity changed")
		}
		return writeMessages(peer, wire.Message{Header: wire.Header{Command: wire.OplockBreak, MessageID: header.MessageID, SessionID: header.SessionID, Flags: wire.FlagResponse}, Body: wantBody})
	})
	if err := client.SendLeaseBreakAcknowledgment(t.Context(), header, ack); err != nil {
		t.Fatal(err)
	}
	reply, err := client.Receive(t.Context())
	if err != nil || len(reply.Messages) != 1 || reply.Messages[0].Header.MessageID != 18 {
		t.Fatalf("acknowledgment reply = %+v, error = %v", reply, err)
	}
}

func TestWaitLeaseBreakCancellation(t *testing.T) {
	client := fakePeer(t, func(net.Conn) error { return nil })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.WaitLeaseBreak(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v", err)
	}
}
