package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// newQueryInfoFixtureWithStorage keeps real object storage while allowing
// configured capacity and a fault injected at the StatFS seam.
func newQueryInfoFixtureWithStorage(t *testing.T, storage smb.Storage) *queryInfoFixture {
	t.Helper()
	options := testOptions(t)
	options.Storage = storage
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	local, remote := net.Pipe()
	client, err := smbtest.NewClient(remote)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, local) }()
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
		select {
		case serveErr := <-done:
			if serveErr != nil && ctx.Err() == nil {
				t.Error(serveErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("ServeConn did not stop")
		}
		if shutdownErr := server.Shutdown(context.WithoutCancel(ctx)); shutdownErr != nil {
			t.Error(shutdownErr)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: options.ShareName, Account: options.Account, Cipher: smb.CipherAES128GCM, Signing: smb.SigningGMAC, ClientGUID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	return &queryInfoFixture{storage: storage, server: server, client: client, ctx: ctx, session: session, next: session.NextMessageID}
}

func TestFilesystemQueriesOnWire(t *testing.T) {
	for _, capacity := range []uint64{0, (3 << 30) + 1023} {
		f := newQueryInfoFixtureWithStorage(t, newFilesMetaStorageWithCapacity(t, capacity))
		_, first := f.create(t, "volume-first", smb.KindFile)
		_, second := f.create(t, "volume-second", smb.KindFile)
		space, err := f.storage.StatFS(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if capacity != 0 && space.Capacity != capacity {
			t.Fatalf("capacity = %d, want %d", space.Capacity, capacity)
		}
		if capacity == 0 && space.Free != 1<<40 {
			t.Fatalf("default free = %d, want 1 TiB", space.Free)
		}
		for _, class := range []wire.FilesystemInfoClass{
			wire.ClassFilesystemVolume, wire.ClassFilesystemSize, wire.ClassFilesystemFullSize,
			wire.ClassFilesystemDevice, wire.ClassFilesystemAttribute,
		} {
			data := queryData(t, f.query(t, first, wire.InfoFilesystem, uint8(class), 1024), smb.StatusSuccess)
			other := queryData(t, f.query(t, second, wire.InfoFilesystem, uint8(class), 1024), smb.StatusSuccess)
			if !bytes.Equal(data, other) {
				t.Fatalf("class %d depends on the queried file", class)
			}
			switch class {
			case wire.ClassFilesystemVolume:
				decodeQueryClass(t, data, wire.DecodeFilesystemVolumeInformation, wire.FilesystemVolumeInformation{
					Label: "backup", Serial: uint32(space.VolumeID & 0xffffffff),
				})
			case wire.ClassFilesystemSize, wire.ClassFilesystemFullSize:
				checkFilesystemSpace(t, data, class, space)
			case wire.ClassFilesystemDevice:
				decodeQueryClass(t, data, wire.DecodeFilesystemDeviceInformation, wire.FilesystemDeviceInformation{Type: 7, Characteristics: 0x10})
			case wire.ClassFilesystemAttribute:
				decodeQueryClass(t, data, wire.DecodeFilesystemAttributeInformation, wire.FilesystemAttributeInformation{
					Name: "s3-smb", Attributes: smb.AdvertisedFilesystemAttributes, MaxComponentLength: 255,
				})
			}
		}
		f.echo(t)
	}
}

func checkFilesystemSpace(t *testing.T, data []byte, class wire.FilesystemInfoClass, space smb.Space) {
	t.Helper()
	if got := binary.LittleEndian.Uint64(data[0:8]); got != space.Capacity/4096 {
		t.Fatalf("class %d total units = %d, want %d", class, got, space.Capacity/4096)
	}
	if got := binary.LittleEndian.Uint64(data[8:16]); got != space.Available/4096 {
		t.Fatalf("class %d caller units = %d, want %d", class, got, space.Available/4096)
	}
	sectorOffset := 16
	if class == wire.ClassFilesystemFullSize {
		if got := binary.LittleEndian.Uint64(data[16:24]); got != space.Free/4096 {
			t.Fatalf("actual available at offset 16 = %d, want %d", got, space.Free/4096)
		}
		sectorOffset = 24
	}
	if len(data) != sectorOffset+8 || binary.LittleEndian.Uint32(data[sectorOffset:sectorOffset+4]) != 8 || binary.LittleEndian.Uint32(data[sectorOffset+4:sectorOffset+8]) != 512 {
		t.Fatalf("class %d allocation geometry = %x", class, data)
	}
}

func TestUnsupportedFilesystemQueriesKeepConnection(t *testing.T) {
	f := newQueryInfoFixture(t)
	_, file := f.create(t, "fs-query", smb.KindFile)
	root, err := f.storage.Lookup(f.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	directory := f.open(t, root.Object)
	for _, open := range []state.Open{file, directory} {
		for class := uint16(0); class <= 255; class++ {
			switch wire.FilesystemInfoClass(class) {
			case wire.ClassFilesystemVolume, wire.ClassFilesystemSize, wire.ClassFilesystemFullSize, wire.ClassFilesystemDevice, wire.ClassFilesystemAttribute:
				continue
			}
			want := smb.StatusInvalidInfoClass
			if class >= 1 && class <= 11 {
				want = smb.StatusNotSupported
			}
			message := f.query(t, open, wire.InfoFilesystem, uint8(class), 1024)
			if message.Header.Status != want {
				t.Fatalf("class %d: status = %x, want %x", class, message.Header.Status, want)
			}
			if _, err := wire.DecodeErrorResponse(message); err != nil {
				t.Fatal(err)
			}
			f.echo(t)
		}
	}
}
