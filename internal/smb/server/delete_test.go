package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestDeleteOnCloseRequiresGrantedDeleteAccess(t *testing.T) {
	for _, test := range []struct {
		name            string
		options, access uint32
		want            smb.Status
	}{
		{name: "ordinary open", access: 1, want: smb.StatusSuccess},
		{name: "delete denied", options: 0x1000, access: 1, want: smb.StatusAccessDenied},
		{name: "generic mask is not a grant", options: 0x1000, access: 0x10000000, want: smb.StatusAccessDenied},
		{name: "delete granted", options: 0x1000, access: 0x10001, want: smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			if status := checkDeleteOnClose(test.options, test.access); status != test.want {
				t.Fatalf("status = %#x, want %#x", status, test.want)
			}
		})
	}
}

func TestCleanupRemovesCurrentNameAndSelectedStream(t *testing.T) {
	for _, stream := range []string{"", "user.AFP_Resource"} {
		t.Run(stream, func(t *testing.T) {
			options := testOptions(t)
			storage := &deletionStorage{}
			options.Storage = storage
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			name := smb.Name{Parent: 1, Base: "selected", Stream: stream}
			action := state.CloseAction{Object: smb.ObjectKey{Inode: 2, Stream: stream}, Name: name, Remove: true}
			if err := server.cleanup(t.Context(), []state.CloseAction{action}); err != nil {
				t.Fatal(err)
			}
			want := smb.Name{Parent: 1, Base: "renamed", Stream: stream}
			if storage.name != want || storage.inode != 2 {
				t.Fatalf("removed %+v inode %d, want %+v inode 2", storage.name, storage.inode, want)
			}
		})
	}
}

func TestCleanupReleasesDeletePendingOnEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "remove failure", "cancelled", "gone inode", "path failure", "lookup failure", "changed inode"} {
		t.Run(outcome, func(t *testing.T) { checkCleanupOutcome(t, outcome) })
	}
}
