package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

type sharingClient struct {
	ctx     context.Context
	client  *smbtest.Client
	done    chan error
	session smbtest.Session
	next    uint64
}

func newSharingServer(t *testing.T) *Server {
	t.Helper()
	options := testOptions(t)
	options.Storage = newFilesMetaStorage(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	return server
}

func newSharingClient(t *testing.T, server *Server) *sharingClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	peer := &sharingClient{client: client, ctx: ctx, done: make(chan error, 1)}
	go func() { peer.done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() { peer.drop(t); cancel() })
	peer.session, err = client.Login(ctx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningCMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	peer.next = peer.session.NextMessageID
	return peer
}

func (peer *sharingClient) drop(t *testing.T) {
	t.Helper()
	if peer.done == nil {
		return
	}
	if err := peer.client.Close(); err != nil {
		t.Error(err)
	}
	select {
	case err := <-peer.done:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sharing connection did not stop")
	}
	peer.done = nil
}

func (peer *sharingClient) create(t *testing.T, request wire.CreateRequest) wire.Message {
	t.Helper()
	response := fileCreate(peer.ctx, t, peer.client, peer.session, peer.next, request)
	peer.next++
	return response
}

func (peer *sharingClient) open(t *testing.T, name string, access, sharing uint32) wire.FileID {
	t.Helper()
	return createdFile(t, peer.create(t, wire.CreateRequest{Name: name, DesiredAccess: access, ShareAccess: sharing, Disposition: fileOpenIf})).ID
}

func (peer *sharingClient) close(t *testing.T, id wire.FileID) {
	t.Helper()
	response := fileClose(peer.ctx, t, peer.client, peer.session, peer.next, id, 0)
	peer.next++
	if response.Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE status = %#x", response.Header.Status)
	}
}

func sharingAccess(rights uint32) uint32 {
	access := rights & 3
	if rights&4 != 0 {
		access |= fileDelete
	}
	return access
}

// MS-FSA 2.1.5.1.2.2 compares both directions, but only if both opens
// request data, execute, append or delete access.
func expectedSharing(firstAccess, firstShare, secondAccess, secondShare uint32) smb.Status {
	if firstAccess == 0 || secondAccess == 0 {
		return smb.StatusSuccess
	}
	if secondAccess&^firstShare != 0 || firstAccess&^secondShare != 0 {
		return smb.StatusSharingViolation
	}
	return smb.StatusSuccess
}

// The exhaustive 4096-case matrix runs against state.Table. This raw subset
// covers every requested-rights mask and sharing mask in both open orders.
func TestSharingRawMatrix(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			for access := uint32(0); access < 8; access++ {
				for share := uint32(0); share < 8; share++ {
					for _, reverse := range []bool{false, true} {
						firstAccess, firstShare := access, uint32(7)
						secondAccess, secondShare := uint32(7), share
						if reverse {
							firstAccess, secondAccess = secondAccess, firstAccess
							firstShare, secondShare = secondShare, firstShare
						}
						checkSharingPair(t, first, second, firstAccess, firstShare, secondAccess, secondShare)
					}
				}
			}
		})
	}
}

func checkSharingPair(t *testing.T, first, second *sharingClient, firstAccess, firstShare, secondAccess, secondShare uint32) {
	t.Helper()
	id := first.open(t, "matrix", sharingAccess(firstAccess), firstShare)
	response := second.create(t, wire.CreateRequest{Name: "matrix", DesiredAccess: sharingAccess(secondAccess), ShareAccess: secondShare, Disposition: fileOpen})
	want := expectedSharing(firstAccess, firstShare, secondAccess, secondShare)
	if response.Header.Status != want {
		t.Fatalf("first access/share %d/%d, second %d/%d: status %#x, want %#x", firstAccess, firstShare, secondAccess, secondShare, response.Header.Status, want)
	}
	if want == smb.StatusSuccess {
		second.close(t, createdFile(t, response).ID)
	}
	first.close(t, id)
	// Neither a rejected CREATE nor CLOSE may leave sharing behind.
	compatible := second.open(t, "matrix", sharingAccess(7), 0)
	second.close(t, compatible)
}

func TestSharingMetadataAndAppend(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			for _, metadata := range []uint32{0x80, 0x100000, 0x100080} {
				for _, reverse := range []bool{false, true} {
					firstAccess, secondAccess := uint32(fileReadData|fileWriteData|fileDelete), metadata
					if reverse {
						firstAccess, secondAccess = secondAccess, firstAccess
					}
					id := first.open(t, "metadata", firstAccess, 0)
					other := second.open(t, "metadata", secondAccess, 0)
					second.close(t, other)
					first.close(t, id)
				}
			}
			for _, reverse := range []bool{false, true} {
				firstAccess, firstShare := uint32(fileReadData), uint32(5)
				secondAccess, secondShare := uint32(fileAppendData), uint32(7)
				if reverse {
					firstAccess, secondAccess = secondAccess, firstAccess
					firstShare, secondShare = secondShare, firstShare
				}
				id := first.open(t, "append", firstAccess, firstShare)
				response := second.create(t, wire.CreateRequest{Name: "append", DesiredAccess: secondAccess, ShareAccess: secondShare, Disposition: fileOpen})
				if response.Header.Status != smb.StatusSharingViolation {
					t.Fatalf("append sharing status = %#x", response.Header.Status)
				}
				first.close(t, id)
			}
		})
	}
}

func (peer *sharingClient) exchange(t *testing.T, command wire.Command, body []byte) wire.Message {
	t.Helper()
	response := ioRoundTrip(peer.ctx, t, peer.client, ioMessage(peer.session, peer.next, command, body, 1))
	peer.next++
	return response
}

func (peer *sharingClient) write(t *testing.T, id wire.FileID, data []byte) {
	t.Helper()
	body, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: id, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	response := peer.exchange(t, wire.Write, body)
	requireIOStatus(t, response, smb.StatusSuccess)
	written, err := wire.DecodeWriteResponse(response)
	if err != nil || uint64(written.Count) != uint64(len(data)) {
		t.Fatalf("WRITE: %+v, %v", written, err)
	}
}

func (peer *sharingClient) read(t *testing.T, id wire.FileID, want string) {
	t.Helper()
	body, err := wire.EncodeReadRequest(wire.ReadRequest{ID: id, Length: smb.CreditUnit})
	if err != nil {
		t.Fatal(err)
	}
	response := peer.exchange(t, wire.Read, body)
	requireIOStatus(t, response, smb.StatusSuccess)
	read, err := wire.DecodeReadResponse(response)
	if err != nil || string(read.Data) != want {
		t.Fatalf("READ data = %q, want %q, error %v", read.Data, want, err)
	}
}

func TestSharingRejectedDestructiveCreatePreservesBytes(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			id := first.open(t, "preserved", fileReadData|fileWriteData, 1)
			const data = "existing bytes must survive"
			first.write(t, id, []byte(data))
			for _, disposition := range []uint32{fileOverwrite, fileOverwriteIf, fileSupersede} {
				response := second.create(t, wire.CreateRequest{Name: "preserved", DesiredAccess: fileWriteData | fileDelete, ShareAccess: 7, Disposition: disposition})
				requireIOStatus(t, response, smb.StatusSharingViolation)
				first.read(t, id, data)
				compatible := second.open(t, "preserved", fileReadData, 7)
				second.read(t, compatible, data)
				second.close(t, compatible)
			}
			first.close(t, id)
			// Once the blocker closes, a destructive open is allowed again.
			response := second.create(t, wire.CreateRequest{Name: "preserved", DesiredAccess: fileWriteData, ShareAccess: 7, Disposition: fileOverwrite})
			created := createdFile(t, response)
			if created.Size != 0 {
				t.Fatalf("allowed overwrite size = %d", created.Size)
			}
			second.close(t, created.ID)
		})
	}
}

type sharingFailureStorage struct {
	smb.Storage
	closed   atomic.Uint64
	failAttr atomic.Bool
}

func (storage *sharingFailureStorage) GetAttr(ctx context.Context, key smb.ObjectKey) (smb.Attr, error) {
	if storage.failAttr.Swap(false) {
		return smb.Attr{}, smb.ErrIO
	}
	return storage.Storage.GetAttr(ctx, key)
}

func (storage *sharingFailureStorage) Close(ctx context.Context, handle smb.Handle) error {
	storage.closed.Add(1)
	return storage.Storage.Close(ctx, handle)
}

func TestSharingFailedCreateAbortsReservationAndClosesHandle(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			storage := &sharingFailureStorage{Storage: server.options.Storage}
			server.options.Storage = storage
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			id := first.open(t, "failed", fileReadData, 7)
			storage.failAttr.Store(true)
			response := second.create(t, wire.CreateRequest{Name: "failed", DesiredAccess: fileWriteData, ShareAccess: 1, Disposition: fileOpen})
			requireIOStatus(t, response, smb.StatusIODeviceError)
			if storage.closed.Load() != 1 {
				t.Fatalf("failed CREATE closed %d storage handles, want 1", storage.closed.Load())
			}
			// The failed open's deny-write mode must not remain reserved.
			compatible := second.open(t, "failed", fileWriteData, 7)
			second.close(t, compatible)
			first.close(t, id)
		})
	}
}

func TestSharingCloseAndDropReleaseAllOpens(t *testing.T) {
	server := newSharingServer(t)
	first := newSharingClient(t, server)
	second := newSharingClient(t, server)
	for _, name := range []string{"close", "drop-one", "drop-two"} {
		id := first.open(t, name, fileReadData, 1)
		other := first.open(t, name, fileReadData, 1)
		for _, closing := range []wire.FileID{id, other} {
			response := second.create(t, wire.CreateRequest{Name: name, DesiredAccess: fileWriteData, ShareAccess: 7, Disposition: fileOpen})
			if response.Header.Status != smb.StatusSharingViolation {
				t.Fatalf("live open lost sharing: %#x", response.Header.Status)
			}
			if name == "close" {
				first.close(t, closing)
			}
		}
		if name == "close" {
			second.close(t, second.open(t, name, fileWriteData, 7))
		}
	}
	first.drop(t)
	for _, name := range []string{"drop-one", "drop-two"} {
		second.close(t, second.open(t, name, fileWriteData, 0))
	}
}

func TestSharingNamedStreams(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			first.close(t, first.open(t, "streams", fileReadData, 7))
			id := first.open(t, "streams:one", fileReadData|fileWriteData, 0)
			other := second.open(t, "streams:two", fileReadData|fileWriteData, 0)
			second.close(t, other)
			response := second.create(t, wire.CreateRequest{Name: "streams", DesiredAccess: fileDelete, ShareAccess: 7, Disposition: fileOpen})
			if response.Header.Status != smb.StatusSharingViolation {
				t.Fatalf("stream deny-delete status = %#x", response.Header.Status)
			}
			first.close(t, id)
			base := second.open(t, "streams", fileDelete, 7)
			response = first.create(t, wire.CreateRequest{Name: "streams:one", DesiredAccess: fileReadData, ShareAccess: 3, Disposition: fileOpen})
			if response.Header.Status != smb.StatusSharingViolation {
				t.Fatalf("reverse stream deny-delete status = %#x", response.Header.Status)
			}
			second.close(t, base)
			first.close(t, first.open(t, "streams:one", fileReadData, 0))
		})
	}
}
