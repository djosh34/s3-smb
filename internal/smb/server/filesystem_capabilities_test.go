package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestFilesystemRefusesUnimplementedCapabilities(t *testing.T) {
	server := newCapabilityServer(t)
	client := capabilityConnection(t, server)
	attributes := client.filesystemAttributes(t)
	const unsupported = 0x00000040 | 0x00400000 | 0x01000000 // Sparse files, hard links, open-by-ID.
	if attributes&unsupported != 0 || attributes != smb.AdvertisedFilesystemAttributes {
		t.Fatalf("filesystem attributes = %#x", attributes)
	}
}

func TestFilesystemSetSparseRefused(t *testing.T) {
	server := newCapabilityServer(t)
	client := capabilityConnection(t, server)
	created := client.create(t, wire.CreateRequest{
		Name: "not-sparse", Disposition: fileCreateDisposition, DesiredAccess: fileAllAccess, ShareAccess: 7,
	}, smb.StatusSuccess)
	body, err := wire.EncodeIOCTLRequest(wire.IOCTLRequest{
		ID: created.ID, ControlCode: 0x000900c4, Input: []byte{1}, Flags: 1, // FSCTL_SET_SPARSE.
	})
	if err != nil {
		t.Fatal(err)
	}
	// IOCTL is not registered. Its dispatcher refusal must not grant sparse state.
	client.exchange(t, wire.IOCTL, body, smb.StatusNotSupported)
	basic, err := wire.DecodeFileBasicInformation(client.fileInformation(t, created.ID, wire.ClassFileBasic))
	if err != nil {
		t.Fatal(err)
	}
	if basic.Attributes&0x00000200 != 0 { // FILE_ATTRIBUTE_SPARSE_FILE.
		t.Fatalf("file attributes = %#x", basic.Attributes)
	}
	client.close(t, created.ID)
}

func TestFilesystemClientSettableAttributes(t *testing.T) {
	const unsupported = uint32(0x200 | 0x400 | 0x800 | 0x4000) // Sparse, reparse, compressed, encrypted.
	type attributeCase struct {
		attributes uint32
		want       uint32
	}
	cases := []attributeCase{
		{0x200, 0x80},
		{0x400, 0x80},
		{0x800, 0x80},
		{0x4000, 0x80},
		{unsupported, 0x80},
		{unsupported | 0x3127, 0x3127}, // All seven client-settable bits.
	}
	for _, bit := range []uint32{1, 2, 4, 0x20, 0x100, 0x1000, 0x2000} {
		cases = append(cases, attributeCase{unsupported | bit, bit})
	}
	for _, test := range cases {
		t.Run(fmt.Sprintf("%x", test.attributes), func(t *testing.T) {
			server := newCapabilityServer(t)
			client := capabilityConnection(t, server)
			created := client.create(t, wire.CreateRequest{
				Name: "attributes", Disposition: fileCreateDisposition, DesiredAccess: fileAllAccess,
				ShareAccess: 7, FileAttributes: test.attributes,
			}, smb.StatusSuccess)
			if created.Attributes != test.want {
				t.Fatalf("CREATE attributes = %#x, want %#x", created.Attributes, test.want)
			}
			assertBasic := func(want uint32) {
				t.Helper()
				basic, err := wire.DecodeFileBasicInformation(client.fileInformation(t, created.ID, wire.ClassFileBasic))
				if err != nil {
					t.Fatal(err)
				}
				if basic.Attributes != want {
					t.Fatalf("Basic attributes = %#x, want %#x", basic.Attributes, want)
				}
			}
			assertBasic(test.want)
			for _, attributes := range []uint32{0x80, test.attributes, 0} {
				input, err := wire.EncodeFileBasicInformation(wire.FileBasicInformation{Attributes: attributes})
				if err != nil {
					t.Fatal(err)
				}
				body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{
					ID: created.ID, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileBasic), Input: input,
				})
				if err != nil {
					t.Fatal(err)
				}
				client.exchange(t, wire.SetInfo, body, smb.StatusSuccess)
				want := test.want
				if attributes == 0x80 {
					want = 0x80
				}
				assertBasic(want)
			}
			client.close(t, created.ID)
		})
	}
}

func TestFilesystemOverwriteAttributes(t *testing.T) {
	for _, disposition := range []uint32{fileOverwrite, fileOverwriteIf, fileSupersede} {
		t.Run(fmt.Sprint(disposition), func(t *testing.T) {
			server := newCapabilityServer(t)
			client := capabilityConnection(t, server)
			request := wire.CreateRequest{
				Name: "overwrite", Disposition: fileCreateDisposition, DesiredAccess: fileAllAccess,
				ShareAccess: 7, FileAttributes: 2, // FILE_ATTRIBUTE_HIDDEN.
			}
			created := client.create(t, request, smb.StatusSuccess)
			client.close(t, created.ID)
			request.Disposition, request.FileAttributes = disposition, 0x200
			opened := client.create(t, request, smb.StatusSuccess)
			want := uint32(2)
			if disposition == fileSupersede {
				want = 0x80
			}
			if opened.Attributes != want {
				t.Fatalf("CREATE attributes = %#x, want %#x", opened.Attributes, want)
			}
			basic, err := wire.DecodeFileBasicInformation(client.fileInformation(t, opened.ID, wire.ClassFileBasic))
			if err != nil {
				t.Fatal(err)
			}
			if basic.Attributes != want {
				t.Fatalf("Basic attributes = %#x, want %#x", basic.Attributes, want)
			}
			client.close(t, opened.ID)
		})
	}
}

func TestFilesystemCaseSensitiveSearch(t *testing.T) {
	server := newCapabilityServer(t)
	client := capabilityConnection(t, server)
	if client.filesystemAttributes(t)&smb.FileCaseSensitiveSearch == 0 {
		t.Fatal("case-sensitive search is not advertised")
	}
	request := wire.CreateRequest{Name: "MixedCase", Disposition: fileCreateDisposition, DesiredAccess: fileAllAccess, ShareAccess: 7}
	upper := client.create(t, request, smb.StatusSuccess)
	client.close(t, upper.ID)
	request.Name, request.Disposition = "mixedcase", fileOpen
	client.create(t, request, smb.StatusObjectNameNotFound)
	request.Disposition = fileCreateDisposition
	lower := client.create(t, request, smb.StatusSuccess)
	client.close(t, lower.ID)
	request.Name, request.Disposition = "MixedCase", fileOpen
	upper = client.create(t, request, smb.StatusSuccess)
	input, err := wire.EncodeFileRenameInformation(wire.FileRenameInformation{Name: "MIXEDCASE"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeSetInfoRequest(wire.SetInfoRequest{
		ID: upper.ID, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileRename), Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.exchange(t, wire.SetInfo, body, smb.StatusSuccess)
	client.close(t, upper.ID)
	client.create(t, request, smb.StatusObjectNameNotFound)
	for _, name := range []string{"MIXEDCASE", "mixedcase"} {
		request.Name = name
		opened := client.create(t, request, smb.StatusSuccess)
		client.close(t, opened.ID)
	}
}

func TestFilesystemPreservedAndUnicodeNames(t *testing.T) {
	for _, test := range []struct {
		name string
		bit  uint32
	}{
		{name: "MiXeD-Preserved", bit: smb.FileCasePreservedNames},
		{name: "資料-😀-café", bit: smb.FileUnicodeOnDisk},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newCapabilityServer(t)
			client := capabilityConnection(t, server)
			if client.filesystemAttributes(t)&test.bit == 0 {
				t.Fatalf("bit %#x is not advertised", test.bit)
			}
			request := wire.CreateRequest{Name: test.name, Disposition: fileCreateDisposition, DesiredAccess: fileAllAccess, ShareAccess: 7}
			created := client.create(t, request, smb.StatusSuccess)
			client.close(t, created.ID)
			request.Disposition = fileOpen
			opened := client.create(t, request, smb.StatusSuccess)
			name, err := wire.DecodeFileNameInformation(client.fileInformation(t, opened.ID, wire.ClassFileName))
			if err != nil {
				t.Fatal(err)
			}
			if name.Name != "\\"+test.name {
				t.Fatalf("SMB name = %q, want %q", name.Name, "\\"+test.name)
			}
			client.close(t, opened.ID)
			// Read the actual adapter's directory, not the client's supplied name.
			root, err := server.options.Storage.Lookup(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			entries, err := server.options.Storage.ReadDir(t.Context(), root.Object.Inode, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				if entry.Name == test.name {
					found = true
				}
			}
			if !found {
				t.Fatalf("stored name missing from directory: %+v", entries)
			}
		})
	}
}
