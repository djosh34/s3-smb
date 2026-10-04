package server

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestIssue97SelectedStreamInformation(t *testing.T) {
	for _, baseSize := range []uint16{0, 4, 128} {
		t.Run(fmt.Sprintf("base-%d", baseSize), func(t *testing.T) {
			c := newStreamClient(t)
			base := c.create(t, streamRequest("data", 2), smb.StatusSuccess).ID
			if baseSize != 0 {
				c.write(t, base, bytes.Repeat([]byte{'b'}, int(baseSize)), 0, smb.StatusSuccess)
			}
			for _, stream := range []string{"AFP_Resource", "AFP_AfpInfo", "com.apple.FinderInfo", "other.xattr"} {
				id := c.create(t, streamRequest("data:"+stream+":$DATA", 2), smb.StatusSuccess).ID
				data := []byte("stream payload")
				c.write(t, id, data, 0, smb.StatusSuccess)
				for _, class := range []wire.FileInfoClass{wire.ClassFileStandard, wire.ClassFileAll, wire.ClassFileNetworkOpen} {
					assertStreamLength(t, c.query(t, id, class), class, uint64(len(data)))
				}
				closed := c.close(t, id)
				if closed.Flags&1 == 0 || closed.Size != uint64(len(data)) {
					t.Fatalf("POSTQUERY_ATTRIB %s: flags %x size %d", stream, closed.Flags, closed.Size)
				}
			}
			assertStreamList(t, c.query(t, base, wire.ClassFileStream), uint64(baseSize))
			resource := c.create(t, streamRequest("data:AFP_Resource", fileOpen), smb.StatusSuccess).ID
			assertStreamList(t, c.query(t, resource, wire.ClassFileStream), uint64(baseSize))
			c.close(t, resource)
			assertStreamLength(t, c.query(t, base, wire.ClassFileStandard), wire.ClassFileStandard, uint64(baseSize))
			c.close(t, base)
		})
	}
}

func assertStreamList(t *testing.T, data []byte, baseSize uint64) {
	t.Helper()
	listed, err := wire.DecodeFileStreamInformation(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{"::$DATA": baseSize, ":AFP_Resource:$DATA": 14, ":AFP_AfpInfo:$DATA": 14, ":com.apple.FinderInfo:$DATA": 14, ":other.xattr:$DATA": 14}
	if len(listed.Entries) != len(want) {
		t.Fatalf("streams: %+v", listed.Entries)
	}
	for _, entry := range listed.Entries {
		size, exists := want[entry.Name]
		if !exists || size != entry.Size || entry.AllocationSize < size {
			t.Fatalf("unexpected stream: %+v", entry)
		}
		delete(want, entry.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing streams: %v", want)
	}
}

func assertStreamLength(t *testing.T, data []byte, class wire.FileInfoClass, want uint64) {
	t.Helper()
	var size, allocation uint64
	switch uint8(class) {
	case uint8(wire.ClassFileStandard):
		info, err := wire.DecodeFileStandardInformation(data)
		if err != nil {
			t.Fatal(err)
		}
		size, allocation = info.EndOfFile, info.AllocationSize
		if info.Directory {
			t.Fatal("stream reported as directory")
		}
	case uint8(wire.ClassFileAll):
		info, err := wire.DecodeFileAllInformation(data)
		if err != nil {
			t.Fatal(err)
		}
		size, allocation = info.Standard.EndOfFile, info.Standard.AllocationSize
	case uint8(wire.ClassFileNetworkOpen):
		info, err := wire.DecodeFileNetworkOpenInformation(data)
		if err != nil {
			t.Fatal(err)
		}
		size, allocation = info.EndOfFile, info.AllocationSize
	default:
		t.Fatalf("unexpected class %d", class)
	}
	if size != want || allocation < size {
		t.Fatalf("class %d: EOF %d allocation %d, want EOF %d", class, size, allocation, want)
	}
}

// Research #165 cites Samba's fruit_create_file. Samba 4.23.0 applies the rule
// to every named stream, only for FILE_OPEN and only after AAPL negotiation:
// https://github.com/samba-team/samba/blob/samba-4.23.0/source3/modules/vfs_fruit.c#L4379-L4392
// An empty unnamed file and FILE_OPEN_IF are not covered by that rule.
func TestAAPLEmptyStreamOpen(t *testing.T) {
	c := newStreamClient(t)
	base := c.create(t, streamRequest("data", fileCreateDisposition), smb.StatusSuccess).ID
	for _, stream := range []string{"AFP_AfpInfo", "AFP_Resource", "com.apple.FinderInfo", "other.xattr"} {
		name := "data:" + stream
		id := c.create(t, streamRequest(name, 2), smb.StatusSuccess).ID
		c.close(t, id)
		id = c.create(t, streamRequest(name, 1), smb.StatusSuccess).ID
		c.close(t, id)
	}
	query, err := wire.EncodeAAPLQuery(wire.AAPLQuery{Requested: 2})
	if err != nil {
		t.Fatal(err)
	}
	request := streamRequest("data", 1)
	request.Contexts = []wire.CreateContext{query}
	negotiated := c.create(t, request, smb.StatusSuccess)
	if len(negotiated.Contexts) != 1 || negotiated.Contexts[0].Name != "AAPL" {
		t.Fatalf("AAPL not negotiated: %+v", negotiated.Contexts)
	}
	c.close(t, negotiated.ID)
	client, ctx, session := newFilesMetaClient(t, c.server)
	fresh := &streamClient{server: c.server, client: client, ctx: ctx, session: session, next: session.NextMessageID}
	freshID := fresh.create(t, streamRequest("data:AFP_Resource", fileOpen), smb.StatusSuccess).ID
	fresh.close(t, freshID)
	for _, stream := range []string{"AFP_AfpInfo", "AFP_Resource", "com.apple.FinderInfo", "other.xattr"} {
		name := "data:" + stream
		c.create(t, streamRequest(name, 1), smb.StatusObjectNameNotFound)
		c.create(t, streamRequest(name, 2), smb.StatusObjectNameCollision)
		id := c.create(t, streamRequest(name, 3), smb.StatusSuccess).ID
		c.write(t, id, []byte("x"), 0, smb.StatusSuccess)
		c.close(t, id)
		id = c.create(t, streamRequest(name, 1), smb.StatusSuccess).ID
		c.read(t, id, 0, []byte("x"))
		c.close(t, id)
	}
	id := c.create(t, streamRequest("data", 1), smb.StatusSuccess).ID
	c.close(t, id)
	c.close(t, base)
}
