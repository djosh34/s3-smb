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
			want := map[string]uint64{"::$DATA": uint64(baseSize), ":AFP_Resource:$DATA": 14, ":AFP_AfpInfo:$DATA": 14, ":com.apple.FinderInfo:$DATA": 14, ":other.xattr:$DATA": 14}
			assertStreamList(t, c.query(t, base, wire.ClassFileStream), want)
			resource := c.create(t, streamRequest("data:AFP_Resource", fileOpen), smb.StatusSuccess).ID
			assertStreamList(t, c.query(t, resource, wire.ClassFileStream), want)
			c.close(t, resource)
			assertStreamLength(t, c.query(t, base, wire.ClassFileStandard), wire.ClassFileStandard, uint64(baseSize))
			c.close(t, base)
		})
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
	populated := c.create(t, streamRequest("data:populated", fileCreateDisposition), smb.StatusSuccess).ID
	c.write(t, populated, []byte("x"), 0, smb.StatusSuccess)
	before := map[string]uint64{"::$DATA": 0, ":populated:$DATA": 1, ":AFP_AfpInfo:$DATA": 0, ":AFP_Resource:$DATA": 0, ":com.apple.FinderInfo:$DATA": 0, ":other.xattr:$DATA": 0}
	assertStreamList(t, c.query(t, base, wire.ClassFileStream), before)
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
	// fruit_filter_empty_streams keeps the unnamed stream, even when empty:
	// https://github.com/samba-team/samba/blob/samba-4.23.0/source3/modules/vfs_fruit.c#L4043-L4071
	after := map[string]uint64{"::$DATA": 0, ":populated:$DATA": 1}
	assertStreamList(t, c.query(t, base, wire.ClassFileStream), after)
	assertStreamList(t, c.query(t, populated, wire.ClassFileStream), after)
	c.read(t, populated, 0, []byte("x"))
	c.close(t, populated)
	populated = c.create(t, streamRequest("data:populated", fileOpen), smb.StatusSuccess).ID
	c.close(t, populated)
	client, ctx, session := newFilesMetaClient(t, c.server)
	fresh := &streamClient{server: c.server, client: client, ctx: ctx, session: session, next: session.NextMessageID}
	freshID := fresh.create(t, streamRequest("data:AFP_Resource", fileOpen), smb.StatusSuccess).ID
	fresh.close(t, freshID)
	freshBase := fresh.create(t, streamRequest("data", fileOpen), smb.StatusSuccess).ID
	assertStreamList(t, fresh.query(t, freshBase, wire.ClassFileStream), before)
	fresh.close(t, freshBase)
	for _, stream := range []string{"AFP_AfpInfo", "AFP_Resource", "com.apple.FinderInfo", "other.xattr"} {
		name := "data:" + stream
		c.create(t, streamRequest(name, 1), smb.StatusObjectNameNotFound)
		directory := streamRequest(name, fileOpen)
		directory.Options = fileDirectoryFile
		c.create(t, directory, smb.StatusNotADirectory)
		c.create(t, streamRequest(name, 2), smb.StatusObjectNameCollision)
		id := c.create(t, streamRequest(name, 3), smb.StatusSuccess).ID
		c.write(t, id, []byte("x"), 0, smb.StatusSuccess)
		c.close(t, id)
		id = c.create(t, streamRequest(name, 1), smb.StatusSuccess).ID
		c.read(t, id, 0, []byte("x"))
		c.close(t, id)
	}
	id := c.create(t, streamRequest("data::$DATA", fileOpen), smb.StatusSuccess).ID
	c.close(t, id)
	c.close(t, base)
}
