package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestIssue94StreamDispositions(t *testing.T) {
	c := newStreamClient(t)
	for _, stream := range []string{"AFP_Resource", "AFP_AfpInfo", "com.apple.FinderInfo", "other.xattr"} {
		for disposition := uint32(0); disposition <= 5; disposition++ {
			// A stream requires its base, so these are the three possible states.
			for _, presence := range []string{"no-base", "base-only", "stream"} {
				t.Run(fmt.Sprintf("%s/%d/%s", stream, disposition, presence), func(t *testing.T) {
					testStreamDisposition(t, c, stream, disposition, presence)
				})
			}
		}
	}
}

func TestStreamOffsetsResizeAndLimit(t *testing.T) {
	c := newStreamClient(t)
	baseData := []byte("ordinary file content stays unchanged")
	base := c.create(t, streamRequest("forked", 2), smb.StatusSuccess).ID
	c.write(t, base, baseData, 0, smb.StatusSuccess)
	for _, stream := range []string{"AFP_Resource", "AFP_AfpInfo", "com.apple.FinderInfo", "other.xattr"} {
		t.Run(stream, func(t *testing.T) {
			id := c.create(t, streamRequest("forked:"+stream, 2), smb.StatusSuccess).ID
			c.write(t, id, []byte("abcdef"), 0, smb.StatusSuccess)
			c.write(t, id, []byte("XY"), 2, smb.StatusSuccess)
			c.read(t, id, 0, []byte("abXYef"))
			c.read(t, id, 2, []byte("XY"))
			c.resize(t, id, 3, smb.StatusSuccess)
			c.resize(t, id, 6, smb.StatusSuccess)
			c.read(t, id, 0, []byte{'a', 'b', 'X', 0, 0, 0})
			c.write(t, id, []byte("Z"), 8, smb.StatusSuccess)
			c.read(t, id, 3, []byte{0, 0, 0, 0, 0, 'Z'})
			c.write(t, id, []byte("!"), smb.MaxStreamSize-1, smb.StatusSuccess)
			c.write(t, id, []byte("!"), smb.MaxStreamSize, smb.StatusFileTooLarge)
			c.write(t, id, []byte("??"), smb.MaxStreamSize-1, smb.StatusFileTooLarge)
			c.resize(t, id, smb.MaxStreamSize+1, smb.StatusFileTooLarge)
			assertStreamLength(t, c.query(t, id, wire.ClassFileStandard), wire.ClassFileStandard, smb.MaxStreamSize)
			c.read(t, id, smb.MaxStreamSize-2, []byte{0, '!'})
			c.resize(t, id, smb.MaxStreamSize, smb.StatusSuccess)
			if closed := c.close(t, id); closed.Size != smb.MaxStreamSize {
				t.Fatalf("CLOSE EOF: %d", closed.Size)
			}
			c.read(t, base, 0, baseData)
		})
	}
	c.close(t, base)
}
