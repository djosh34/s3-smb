package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
)

type droppingDeletionPeer struct {
	peer *deletionPeer
	done chan struct{}
	err  error
}

// This peer exposes transport cleanup completion without shutting down other
// connections. They must keep their handles while the deleting peer drops.
func newDroppingDeletionPeer(t *testing.T, server *Server) *droppingDeletionPeer {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	dropping := &droppingDeletionPeer{done: make(chan struct{})}
	go func() {
		dropping.err = server.ServeConn(ctx, local)
		close(dropping.done)
	}()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
		<-dropping.done
		if dropping.err != nil && !errors.Is(dropping.err, context.Canceled) {
			t.Error(dropping.err)
		}
		if shutdownErr := server.Shutdown(context.WithoutCancel(t.Context())); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: server.options.ShareName, Account: server.options.Account, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{3}})
	if err != nil {
		t.Fatal(err)
	}
	dropping.peer = &deletionPeer{ctx: ctx, client: client, session: session, next: session.NextMessageID}
	return dropping
}

func (dropping *droppingDeletionPeer) drop(t *testing.T) {
	t.Helper()
	if err := dropping.peer.client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dropping.done:
		if dropping.err != nil {
			t.Fatal(dropping.err)
		}
	case <-dropping.peer.ctx.Done():
		t.Fatal(dropping.peer.ctx.Err())
	}
}

func TestDroppedStreamDeletionKeepsOtherConnectionHandles(t *testing.T) {
	for _, other := range []string{"data", "data:stream:$DATA"} {
		t.Run(other, func(t *testing.T) {
			server := deletionServer(t)
			storage := server.options.Storage
			seedDeletionData(t, storage, "data", "base data")
			seedDeletionData(t, storage, "data:stream:$DATA", "stream data")
			seedDeletionData(t, storage, "data:other:$DATA", "other stream")
			dropping := newDroppingDeletionPeer(t, server)
			surviving := newDeletionPeer(t, server)
			held := surviving.open(t, other, fileReadData, fileOpen, 0, smb.StatusSuccess)
			dropping.peer.open(t, "data:stream:$DATA", fileDelete, fileOpen, fileDeleteOnClose, smb.StatusSuccess)
			dropping.drop(t)
			if other == "data:stream:$DATA" {
				requireDeletionData(t, storage, other, "stream data")
				surviving.open(t, other, fileReadData, fileOpen, 0, smb.StatusDeletePending)
			} else {
				requireDeletionMissing(t, storage, "data:stream:$DATA")
			}
			surviving.close(t, held)
			requireDeletionMissing(t, storage, "data:stream:$DATA")
			requireDeletionData(t, storage, "data", "base data")
			requireDeletionData(t, storage, "data:other:$DATA", "other stream")
		})
	}
}
