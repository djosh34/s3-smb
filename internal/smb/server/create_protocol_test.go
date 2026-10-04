package server

import (
	"fmt"
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestCreateRejectsLeadingSeparatorBeforeMutation(t *testing.T) {
	storage := smbtest.NewStorage(t)
	client := newReadWriteClient(t, storage)
	for _, name := range []string{"\\leading", "/leading", "\\\\leading", "//leading"} {
		t.Run(name, func(t *testing.T) {
			requireIOStatus(t, client.create(t, createRequest(name, fileOpenIf)), smb.StatusInvalidParameter)
		})
	}
	selected, err := storage.Lookup(t.Context(), "leading")
	if err != nil || selected.Exists {
		t.Fatalf("rejected CREATE changed namespace: %+v, %v", selected, err)
	}
	body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	requireIOStatus(t, client.exchange(t, wire.Echo, body, 1), smb.StatusSuccess)
}

func TestCreateImpersonationLevels(t *testing.T) {
	storage := smbtest.NewStorage(t)
	client := newReadWriteClient(t, storage)
	for _, level := range []uint32{0, 1, 2, 3, 4, 5, math.MaxUint32} {
		t.Run(fmt.Sprintf("level_%d", level), func(t *testing.T) {
			name := fmt.Sprintf("impersonation-%d", level)
			request := createRequest(name, fileCreateDisposition)
			request.ImpersonationLevel = level
			response := client.create(t, request)
			if level <= 3 {
				opened := createdFile(t, response)
				requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
				return
			}
			requireIOStatus(t, response, smb.StatusBadImpersonationLevel)
			selected, err := storage.Lookup(t.Context(), name)
			if err != nil || selected.Exists {
				t.Fatalf("invalid impersonation created a file: %+v, %v", selected, err)
			}
		})
	}
	body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	requireIOStatus(t, client.exchange(t, wire.Echo, body, 1), smb.StatusSuccess)
}

func TestCreateRegularFileArchiveAttributes(t *testing.T) {
	client := newReadWriteClient(t, smbtest.NewStorage(t))
	for _, disposition := range []uint32{fileSupersede, fileCreateDisposition, fileOpenIf, fileOverwriteIf} {
		for _, attributes := range []uint32{0, 0x80, 0x2, 0x100, 0x200, 0x2082} {
			t.Run(fmt.Sprintf("disposition_%d_attributes_%x", disposition, attributes), func(t *testing.T) {
				name := fmt.Sprintf("new-%d-%x", disposition, attributes)
				request := createRequest(name, disposition)
				request.FileAttributes = attributes
				opened := createdFile(t, client.create(t, request))
				want := attributes&clientSettableFileAttributes | 0x20
				if opened.Attributes != want || opened.Action != 2 {
					t.Fatalf("new file: action %d, attributes %#x, want %#x", opened.Action, opened.Attributes, want)
				}
				requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
				reopened := createdFile(t, client.create(t, createRequest(name, fileOpen)))
				if reopened.Attributes != want {
					t.Fatalf("reopen attributes %#x, want %#x", reopened.Attributes, want)
				}
				requireIOStatus(t, client.close(t, reopened.ID, 0), smb.StatusSuccess)
			})
		}
	}
	request := createRequest("directory-attributes", fileCreateDisposition)
	request.Options = fileDirectoryFile
	opened := createdFile(t, client.create(t, request))
	if opened.Attributes != 0x10 {
		t.Fatalf("directory attributes %#x, want DIRECTORY only", opened.Attributes)
	}
	requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
}

func TestCreateOverwriteSetsArchiveAndPreservesOtherAttributes(t *testing.T) {
	client := newReadWriteClient(t, smbtest.NewStorage(t))
	for _, disposition := range []uint32{fileOverwrite, fileOverwriteIf} {
		for _, attributes := range []uint32{0, 0x80, 0x2} {
			t.Run(fmt.Sprintf("disposition_%d_attributes_%x", disposition, attributes), func(t *testing.T) {
				name := fmt.Sprintf("overwrite-%d-%x", disposition, attributes)
				request := createRequest(name, fileCreateDisposition)
				request.FileAttributes = 0x100
				opened := createdFile(t, client.create(t, request))
				requireIOStatus(t, client.close(t, opened.ID, 0), smb.StatusSuccess)
				request.Disposition, request.FileAttributes = disposition, attributes
				replaced := createdFile(t, client.create(t, request))
				want := attributes&clientSettableFileAttributes | 0x120
				if replaced.Attributes != want || replaced.Action != 3 {
					t.Fatalf("overwrite: action %d, attributes %#x, want %#x", replaced.Action, replaced.Attributes, want)
				}
				requireIOStatus(t, client.close(t, replaced.ID, 0), smb.StatusSuccess)
				reopened := createdFile(t, client.create(t, createRequest(name, fileOpen)))
				if reopened.Attributes != want {
					t.Fatalf("reopened attributes %#x, want %#x", reopened.Attributes, want)
				}
				requireIOStatus(t, client.close(t, reopened.ID, 0), smb.StatusSuccess)
			})
		}
	}
}
