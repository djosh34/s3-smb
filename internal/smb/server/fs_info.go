package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// queryFilesystemInfo returns class data for the request's disk share.
// Open validation belongs to QUERY_INFO; volume information needs no handle.
func queryFilesystemInfo(_ context.Context, _ RequestContext, class uint8, outputLength uint32) ([]byte, smb.Status) {
	if wire.FilesystemInfoClass(class) != wire.ClassFilesystemAttribute {
		return nil, smb.StatusInvalidInfoClass
	}
	if outputLength < 12 {
		return nil, smb.StatusInfoLengthMismatch
	}
	data, err := wire.EncodeFilesystemAttributeInformation(wire.FilesystemAttributeInformation{
		Name: "s3-smb", Attributes: smb.AdvertisedFilesystemAttributes, MaxComponentLength: 255,
	})
	if err != nil {
		return nil, smb.StatusInternalError
	}
	if uint64(outputLength) < uint64(len(data)) {
		return data[:outputLength], smb.StatusBufferOverflow
	}
	return data, smb.StatusSuccess
}
