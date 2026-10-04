package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestLeaseBreakWaitCompletesWhenHolderDisconnects(t *testing.T) {
	server, client, ctx, session, storage := leaseServer(t, smb.CipherAES128GCM, smb.SigningCMAC)
	open := insertLeaseOpen(t, server, session, 3, smb.LeaseRead|smb.LeaseHandle|smb.LeaseWrite, true)
	done := startServerBreak(ctx, server, open, smb.LeaseRead|smb.LeaseHandle)
	if _, err := client.WaitLeaseBreak(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	finishServerBreak(ctx, t, done)
	if storage.closed.Load() != 1 {
		t.Fatalf("detached break cleanup closed %d handles", storage.closed.Load())
	}
}
