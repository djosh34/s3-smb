package smb2

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"syscall"
	"testing"

	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/erref"
	. "github.com/djosh34/s3-smb/internal/smb-old/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
)

func TestResourceForkPayloadNeverLogged(t *testing.T) {
	const marker = "unregistered-resource-fork-secret-54fc"
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	f := &faultFS{deleteDispositionFS: newDeleteDispositionFS(vfs.FileTypeRegularFile), xattrErr: syscall.EIO}
	tree, id, out := wireTree(t, f)
	open := tree.conn.serverCtx.getOpen(7)
	open.isEa = true
	open.eaKey = "AFP_Resource"
	// Ensure the diagnostic route is active, not merely silent or filtered.
	log.Debug("resource-fork regression diagnostic")
	if err := tree.writeImpl(nil, requestBytes(&WriteRequest{FileId: id, Data: []byte(marker)}), id, open, 0); err != nil {
		t.Fatal(err)
	}
	if got := wireStatus(t, out); got != STATUS_IO_DEVICE_ERROR {
		t.Fatal(got)
	}
	if strings.Contains(output.String(), marker) {
		t.Fatal("raw resource fork payload leaked")
	}
	if !strings.Contains(output.String(), "resource-fork regression diagnostic") {
		t.Fatal("logging was not active")
	}
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		if !json.Valid(line) {
			t.Fatalf("invalid JSON diagnostic %q", line)
		}
	}
}
