package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestIOCTLConnectionScopedSentinelRefused(t *testing.T) {
	client := capabilityConnection(t, newCapabilityServer(t))
	for _, test := range []struct {
		name  string
		code  uint32
		flags uint32
	}{
		{"dfs_referrals", 0x00060194, 1},
		{"dfs_referrals_ex", 0x000601b0, 1},
		{"pipe_wait", 0x00110018, 1},
		{"network_interfaces", 0x001401fc, 1},
		{"validate_negotiate", 0x00140204, 1},
		{"not_fsctl", 0x000900c0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := wire.EncodeIOCTLRequest(wire.IOCTLRequest{
				ID: placeholderFileID(), ControlCode: test.code, Flags: test.flags,
			})
			if err != nil {
				t.Fatal(err)
			}
			client.exchange(t, wire.IOCTL, body, smb.StatusNotSupported)
		})
	}
	client.exchange(t, wire.Echo, echo(t, 0).Body, smb.StatusSuccess)
}

func TestIOCTLRefusal(t *testing.T) {
	t.Run("related5_unknown_handle", func(t *testing.T) {
		client := capabilityConnection(t, newCapabilityServer(t))
		body, err := wire.EncodeIOCTLRequest(wire.IOCTLRequest{
			ID: placeholderFileID(), ControlCode: 0x000900c0, Flags: 1, // FSCTL_CREATE_OR_GET_OBJECT_ID.
		})
		if err != nil {
			t.Fatal(err)
		}
		// Samba related5 starts with an unrelated IOCTL, with no predecessor.
		first := ioMessage(client.session, client.next, wire.IOCTL, body, 1)
		closeMessage := compoundFileRequest(t, client.session, wire.Close, client.next+1, placeholderFileID(), true)
		client.next += 2
		responses := exchange(client.ctx, t, client.client, first, closeMessage)
		if len(responses) != 2 {
			t.Fatalf("IOCTL/CLOSE responses = %+v", responses)
		}
		for index, response := range responses {
			if response.Header.Status != smb.StatusFileClosed {
				t.Fatalf("member %d status = %#x, want FILE_CLOSED", index, response.Header.Status)
			}
		}
		client.exchange(t, wire.Echo, echo(t, 0).Body, smb.StatusSuccess)
	})

	t.Run("valid_open", func(t *testing.T) {
		client := capabilityConnection(t, newCapabilityServer(t))
		created := client.create(t, createRequest("object-id", fileCreateDisposition), smb.StatusSuccess)
		body, err := wire.EncodeIOCTLRequest(wire.IOCTLRequest{
			ID: created.ID, ControlCode: 0x000900c0, Flags: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		client.exchange(t, wire.IOCTL, body, smb.StatusNotSupported)
		// CLOSE must not wait for an unreleased refusal-handler reference.
		client.close(t, created.ID)
	})

	t.Run("malformed_body", func(t *testing.T) {
		client := capabilityConnection(t, newCapabilityServer(t))
		body, err := wire.EncodeIOCTLRequest(wire.IOCTLRequest{
			ID: placeholderFileID(), ControlCode: 0x000900c0, Flags: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		client.exchange(t, wire.IOCTL, body[:55], smb.StatusInvalidParameter)
		client.exchange(t, wire.Echo, echo(t, 0).Body, smb.StatusSuccess)
	})
}
