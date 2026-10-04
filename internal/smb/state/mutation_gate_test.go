package state

import (
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

type mutationHandle struct{ object smb.ObjectKey }

func (handle mutationHandle) Key() smb.ObjectKey { return handle.object }

// leaseMutationAllows is the agreed internal seam used by CREATE selection and
// Commit revalidation. Its caller must hold mu; the gate changes no lease state.
func TestLeaseMutationGateExcludesOnlySelectedIdentity(t *testing.T) {
	table, err := New(time.Now)
	if err != nil {
		t.Fatal(err)
	}
	request := OpenRequest{Object: smb.ObjectKey{Inode: 1}, Binding: Binding{SessionID: 1, TreeID: 1}, ClientGUID: GUID{1}, Sharing: 7}
	reservation, status := table.Reserve(request)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	open, status := table.Commit(reservation, Grant{Handle: mutationHandle{object: request.Object}, Lease: Lease{ClientGUID: request.ClientGUID, Key: GUID{4}, State: smb.LeaseRead}})
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	first, _, _, status := table.BeginMutation(open.ID, open.Binding)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	defer table.EndMutation(first)
	second, _, _, status := table.BeginMutation(open.ID, open.Binding)
	if status != smb.StatusSuccess {
		t.Fatal(status)
	}
	defer table.EndMutation(second)
	for _, test := range []struct {
		object  smb.ObjectKey
		client  GUID
		key     GUID
		allowed bool
	}{
		{object: open.Object, client: open.ClientGUID, key: open.LeaseKey, allowed: true},
		{object: open.Object, client: open.ClientGUID, key: GUID{9}},
		{object: open.Object, client: GUID{9}, key: open.LeaseKey},
		{object: smb.ObjectKey{Inode: 2}, client: GUID{9}, key: GUID{9}, allowed: true},
		{object: smb.ObjectKey{Inode: 1, Stream: "fork"}, client: GUID{9}, key: GUID{9}, allowed: true},
	} {
		table.mu.Lock()
		allowed := table.leaseMutationAllows(test.object, test.client, test.key)
		table.mu.Unlock()
		if allowed != test.allowed {
			t.Fatalf("gate (%+v,%x,%x) = %v, want %v", test.object, test.client, test.key, allowed, test.allowed)
		}
	}
	if _, status := table.Close(open.ID, open.Binding); status != smb.StatusSuccess {
		t.Fatal(status)
	}
	table.EndMutation(first)
	table.mu.Lock()
	allowed := table.leaseMutationAllows(open.Object, GUID{9}, GUID{9})
	table.mu.Unlock()
	if allowed {
		t.Fatal("close or one token release erased another active gate")
	}
	table.EndMutation(second)
	table.mu.Lock()
	allowed = table.leaseMutationAllows(open.Object, GUID{9}, GUID{9})
	table.mu.Unlock()
	if !allowed {
		t.Fatal("last token release left grant exclusion behind")
	}
}
