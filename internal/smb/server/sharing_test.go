package server

import (
	"fmt"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// The exhaustive 4096-case matrix runs against state.Table. This raw subset
// covers every requested-rights mask and sharing mask in both open orders.
func TestSharingRawMatrix(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			for access := uint32(0); access < 8; access++ {
				for share := uint32(0); share < 8; share++ {
					for _, reverse := range []bool{false, true} {
						firstAccess, firstShare := access, uint32(7)
						secondAccess, secondShare := uint32(7), share
						if reverse {
							firstAccess, secondAccess = secondAccess, firstAccess
							firstShare, secondShare = secondShare, firstShare
						}
						checkSharingPair(t, first, second, firstAccess, firstShare, secondAccess, secondShare)
					}
				}
			}
		})
	}
}

func TestSharingMetadataAndAppend(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			for _, metadata := range []uint32{0x80, 0x100000, 0x100080} {
				for _, reverse := range []bool{false, true} {
					firstAccess, secondAccess := uint32(fileReadData|fileWriteData|fileDelete), metadata
					if reverse {
						firstAccess, secondAccess = secondAccess, firstAccess
					}
					id := first.open(t, "metadata", firstAccess, 0)
					other := second.open(t, "metadata", secondAccess, 0)
					second.close(t, other)
					first.close(t, id)
				}
			}
			for _, reverse := range []bool{false, true} {
				firstAccess, firstShare := uint32(fileReadData), uint32(5)
				secondAccess, secondShare := uint32(fileAppendData), uint32(7)
				if reverse {
					firstAccess, secondAccess = secondAccess, firstAccess
					firstShare, secondShare = secondShare, firstShare
				}
				id := first.open(t, "append", firstAccess, firstShare)
				response := second.create(t, wire.CreateRequest{Name: "append", DesiredAccess: secondAccess, ShareAccess: secondShare, Disposition: fileOpen})
				if response.Header.Status != smb.StatusSharingViolation {
					t.Fatalf("append sharing status = %#x", response.Header.Status)
				}
				first.close(t, id)
			}
		})
	}
}

func TestSharingRejectedDestructiveCreatePreservesBytes(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			id := first.open(t, "preserved", fileReadData|fileWriteData, 1)
			const data = "existing bytes must survive"
			first.write(t, id, []byte(data))
			for _, disposition := range []uint32{fileOverwrite, fileOverwriteIf, fileSupersede} {
				response := second.create(t, wire.CreateRequest{Name: "preserved", DesiredAccess: fileWriteData | fileDelete, ShareAccess: 7, Disposition: disposition})
				requireIOStatus(t, response, smb.StatusSharingViolation)
				first.read(t, id, data)
				compatible := second.open(t, "preserved", fileReadData, 7)
				second.read(t, compatible, data)
				second.close(t, compatible)
			}
			first.close(t, id)
			// Once the blocker closes, a destructive open is allowed again.
			response := second.create(t, wire.CreateRequest{Name: "preserved", DesiredAccess: fileWriteData, ShareAccess: 7, Disposition: fileOverwrite})
			created := createdFile(t, response)
			if created.Size != 0 {
				t.Fatalf("allowed overwrite size = %d", created.Size)
			}
			second.close(t, created.ID)
		})
	}
}

func TestSharingFailedCreateAbortsReservationAndClosesHandle(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			storage := &sharingFailureStorage{Storage: server.options.Storage}
			server.options.Storage = storage
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			id := first.open(t, "failed", fileReadData, 7)
			storage.failAttr.Store(true)
			response := second.create(t, wire.CreateRequest{Name: "failed", DesiredAccess: fileWriteData, ShareAccess: 1, Disposition: fileOpen})
			requireIOStatus(t, response, smb.StatusIODeviceError)
			if storage.closed.Load() != 1 {
				t.Fatalf("failed CREATE closed %d storage handles, want 1", storage.closed.Load())
			}
			// The failed open's deny-write mode must not remain reserved.
			compatible := second.open(t, "failed", fileWriteData, 7)
			second.close(t, compatible)
			first.close(t, id)
		})
	}
}

func TestSharingCloseAndDropReleaseAllOpens(t *testing.T) {
	server := newSharingServer(t)
	first := newSharingClient(t, server)
	second := newSharingClient(t, server)
	for _, name := range []string{"close", "drop-one", "drop-two"} {
		id := first.open(t, name, fileReadData, 1)
		other := first.open(t, name, fileReadData, 1)
		for _, closing := range []wire.FileID{id, other} {
			response := second.create(t, wire.CreateRequest{Name: name, DesiredAccess: fileWriteData, ShareAccess: 7, Disposition: fileOpen})
			if response.Header.Status != smb.StatusSharingViolation {
				t.Fatalf("live open lost sharing: %#x", response.Header.Status)
			}
			if name == "close" {
				first.close(t, closing)
			}
		}
		if name == "close" {
			second.close(t, second.open(t, name, fileWriteData, 7))
		}
	}
	first.drop(t)
	for _, name := range []string{"drop-one", "drop-two"} {
		second.close(t, second.open(t, name, fileWriteData, 0))
	}
}

func TestSharingMetadataStreamAllowsBaseDelete(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			first.close(t, first.open(t, "metadata", fileReadData, 7))
			first.close(t, first.open(t, "metadata:xattr", fileWriteData, 7))
			for _, metadata := range []uint32{0, 0x80, 0x100000, 0x100080} {
				for _, reverse := range []bool{false, true} {
					firstName, secondName := "metadata:xattr", "metadata"
					firstAccess, secondAccess := metadata, uint32(fileDelete)
					if reverse {
						firstName, secondName = secondName, firstName
						firstAccess, secondAccess = secondAccess, firstAccess
					}
					id := first.open(t, firstName, firstAccess, 0)
					other := second.open(t, secondName, secondAccess, 0)
					second.close(t, other)
					first.close(t, id)
				}
			}
		})
	}
}

func TestSharingNamedStreams(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("two_connections_%t", separate), func(t *testing.T) {
			server := newSharingServer(t)
			first := newSharingClient(t, server)
			second := first
			if separate {
				second = newSharingClient(t, server)
			}
			first.close(t, first.open(t, "streams", fileReadData, 7))
			id := first.open(t, "streams:one", fileReadData|fileWriteData, 0)
			other := second.open(t, "streams:two", fileReadData|fileWriteData, 0)
			second.close(t, other)
			response := second.create(t, wire.CreateRequest{Name: "streams", DesiredAccess: fileDelete, ShareAccess: 7, Disposition: fileOpen})
			if response.Header.Status != smb.StatusSharingViolation {
				t.Fatalf("stream deny-delete status = %#x", response.Header.Status)
			}
			first.close(t, id)
			base := second.open(t, "streams", fileDelete, 7)
			response = first.create(t, wire.CreateRequest{Name: "streams:one", DesiredAccess: fileReadData, ShareAccess: 3, Disposition: fileOpen})
			if response.Header.Status != smb.StatusSharingViolation {
				t.Fatalf("reverse stream deny-delete status = %#x", response.Header.Status)
			}
			second.close(t, base)
			first.close(t, first.open(t, "streams:one", fileReadData, 0))
		})
	}
}
