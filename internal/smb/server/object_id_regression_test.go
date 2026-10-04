package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// FILE_READ_ATTRIBUTES.

func TestObjectIDIOCTLRefusal(t *testing.T) {
	for _, test := range objectIDOpenCases() {
		t.Run(test.name, func(t *testing.T) {
			_, client, ctx, session := newFileClient(t)
			id := session.NextMessageID
			opened := createdFile(t, fileCreate(ctx, t, client, session, id, test.request))
			if opened.ID.Persistent == 0 || opened.ID.Volatile == 0 {
				t.Fatalf("CREATE returned a zero open ID: %+v", opened.ID)
			}

			// MS-FSCC 2.3.1: no input, a file or directory handle, and
			// space for the 64-byte FILE_OBJECTID_BUFFER response.
			body, err := wire.EncodeIOCTLRequest(wire.IOCTLRequest{
				ControlCode: 0x000900c0, ID: opened.ID, MaxOutput: 64, Flags: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			request := wire.Message{Header: wire.Header{
				Command: wire.IOCTL, MessageID: id + 1, SessionID: session.SessionID,
				TreeID: session.TreeID, CreditCharge: 1, Credit: 1,
			}, Body: body}
			response := exchange(ctx, t, client, request)[0]
			// MS-SMB2 3.3.5.15 uses NOT_SUPPORTED when the server does
			// not allow an FSCTL. Object IDs are omitted by policy, not
			// an allowed operation that this filesystem cannot perform.
			if response.Header.Status != smb.StatusNotSupported {
				t.Fatalf("FSCTL_CREATE_OR_GET_OBJECT_ID status = %#x, want NOT_SUPPORTED", response.Header.Status)
			}
			if _, err := wire.DecodeErrorResponse(response); err != nil {
				t.Fatal(err)
			}
			objectIDAssertEcho(ctx, t, client, session, id+2)
			closed := fileClose(ctx, t, client, session, id+3, opened.ID, 0)
			if closed.Header.Status != smb.StatusSuccess {
				t.Fatalf("CLOSE status = %#x", closed.Header.Status)
			}
		})
	}
}

func TestQFidOptionalCreateContext(t *testing.T) {
	for _, test := range objectIDOpenCases() {
		t.Run(test.name, func(t *testing.T) {
			_, client, ctx, session := newFileClient(t)
			query, err := wire.EncodeFileIDQuery(wire.FileIDQuery{})
			if err != nil {
				t.Fatal(err)
			}
			create := test.request
			create.Contexts = []wire.CreateContext{query}
			id := session.NextMessageID
			opened := createdFile(t, fileCreate(ctx, t, client, session, id, create))
			if opened.ID.Persistent == 0 || opened.ID.Volatile == 0 {
				t.Fatalf("CREATE returned a zero open ID: %+v", opened.ID)
			}
			// Decision #409: ignore this optional context without granting
			// object-ID or open-by-ID support.
			for _, responseContext := range opened.Contexts {
				if responseContext.Name == "QFid" {
					t.Fatalf("CREATE returned an unsupported QFid response: %+v", responseContext)
				}
			}
			objectIDAssertEcho(ctx, t, client, session, id+1)
			closed := fileClose(ctx, t, client, session, id+2, opened.ID, 0)
			if closed.Header.Status != smb.StatusSuccess {
				t.Fatalf("CLOSE status = %#x", closed.Header.Status)
			}
		})
	}
}
