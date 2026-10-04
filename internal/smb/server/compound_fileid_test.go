package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// Regression for #145: a signed FLUSH on an existing open supplies the FileId
// for a related FLUSH. CREATE and delayed FLUSH use the same seam.
func TestCompoundFileIDInheritance(t *testing.T) {
	for _, test := range []fileIDCase{
		{name: "existing", prefix: wire.Flush},
		{name: "existing_async", prefix: wire.Flush, async: true},
		{name: "created", prefix: wire.Create},
		{name: "unset_preserves", prefix: wire.Flush, skip: true},
		{name: "unset_preserves_async", prefix: wire.Flush, async: true, skip: true},
		{name: "missing", prefix: wire.Echo, want: smb.StatusInvalidParameter},
		{name: "unrelated_placeholder", prefix: wire.Flush, unrelated: true, want: smb.StatusFileClosed},
	} {
		t.Run(test.name, func(t *testing.T) { checkCompoundFileID(t, test) })
	}
}

func TestCompoundFailedPredecessor(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) { checkCompoundFailedPredecessor(t, async) })
	}
}

func TestCommandsNeedingFileID(t *testing.T) {
	for _, command := range []wire.Command{wire.Close, wire.Read, wire.Write, wire.Flush, wire.Lock, wire.IOCTL, wire.QueryDirectory, wire.ChangeNotify, wire.QueryInfo, wire.SetInfo} {
		if !needsFileID(command) {
			t.Errorf("command %d must inherit predecessor errors", command)
		}
	}
	for _, command := range []wire.Command{wire.Create, wire.Echo, wire.Negotiate, wire.SessionSetup, wire.TreeConnect, wire.TreeDisconnect, wire.Logoff, wire.Cancel, wire.OplockBreak} {
		if needsFileID(command) {
			t.Errorf("command %d does not need a FileId", command)
		}
	}
}
