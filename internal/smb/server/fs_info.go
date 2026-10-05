package server

import (
	"context"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

const (
	filesystemBytesPerSector = 512
	filesystemSectorsPerUnit = 8
	filesystemBytesPerUnit   = filesystemBytesPerSector * filesystemSectorsPerUnit
)

// queryFilesystemInfo returns class data for the request's disk share.
// Open validation belongs to QUERY_INFO; volume information needs no handle.
// It returns an error only when the data cannot be encoded.
func queryFilesystemInfo(ctx context.Context, request RequestContext, class uint8, outputLength uint32) ([]byte, smb.Status, error) {
	infoClass := wire.FilesystemInfoClass(class)
	var minimum uint32
	switch infoClass {
	case wire.ClassFilesystemVolume:
		minimum = 18
	case wire.ClassFilesystemSize:
		minimum = 24
	case wire.ClassFilesystemFullSize:
		minimum = 32
	case wire.ClassFilesystemDevice:
		minimum = 8
	case wire.ClassFilesystemAttribute:
		minimum = 12
	default:
		// MS-FSCC 2.5 defines classes 1 through 11. None of the other
		// defined classes, including object IDs (8), are implemented.
		if class >= 1 && class <= 11 {
			return nil, smb.StatusNotSupported, nil
		}
		return nil, smb.StatusInvalidInfoClass, nil
	}
	if outputLength < minimum {
		return nil, smb.StatusInfoLengthMismatch, nil
	}
	var space smb.Space
	if infoClass == wire.ClassFilesystemVolume || infoClass == wire.ClassFilesystemSize || infoClass == wire.ClassFilesystemFullSize {
		var err error
		space, err = request.Storage.StatFS(ctx)
		if err != nil {
			return nil, smb.StatusFromError(err), nil
		}
	}
	data, err := encodeFilesystemInfo(infoClass, request.Tree.Share, space)
	if err != nil {
		return nil, smb.StatusSuccess, err
	}
	if uint64(outputLength) < uint64(len(data)) {
		return data[:outputLength], smb.StatusBufferOverflow, nil
	}
	return data, smb.StatusSuccess, nil
}

func encodeFilesystemInfo(class wire.FilesystemInfoClass, label string, space smb.Space) ([]byte, error) {
	switch class {
	case wire.ClassFilesystemVolume:
		return wire.EncodeFilesystemVolumeInformation(wire.FilesystemVolumeInformation{
			Label: label, Serial: uint32(space.VolumeID & 0xffffffff),
		})
	case wire.ClassFilesystemSize:
		return wire.EncodeFilesystemSizeInformation(wire.FilesystemSizeInformation{
			TotalUnits: space.Capacity / filesystemBytesPerUnit, AvailableUnits: space.Available / filesystemBytesPerUnit,
			SectorsPerUnit: filesystemSectorsPerUnit, BytesPerSector: filesystemBytesPerSector,
		})
	case wire.ClassFilesystemFullSize:
		return wire.EncodeFilesystemFullSizeInformation(wire.FilesystemFullSizeInformation{
			TotalUnits: space.Capacity / filesystemBytesPerUnit, CallerAvailableUnits: space.Available / filesystemBytesPerUnit,
			ActualAvailableUnits: space.Free / filesystemBytesPerUnit,
			SectorsPerUnit:       filesystemSectorsPerUnit, BytesPerSector: filesystemBytesPerSector,
		})
	case wire.ClassFilesystemDevice:
		return wire.EncodeFilesystemDeviceInformation(wire.FilesystemDeviceInformation{
			Type: 0x07, Characteristics: 0x10, // FILE_DEVICE_DISK, FILE_REMOTE_DEVICE.
		})
	case wire.ClassFilesystemAttribute:
		return wire.EncodeFilesystemAttributeInformation(wire.FilesystemAttributeInformation{
			Name: "s3-smb", Attributes: traceFSAttributes(), MaxComponentLength: 255,
		})
	default:
		return nil, smb.ErrNotSupported
	}
}
