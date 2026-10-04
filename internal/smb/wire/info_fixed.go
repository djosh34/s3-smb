package wire

// DecodeFileBasicInformation reads the MS-FSCC class buffer.
func DecodeFileBasicInformation(data []byte) (FileBasicInformation, error) {
	r := reader{data: data}
	var v FileBasicInformation
	v.Created = Filetime(r.u64())
	v.Accessed = Filetime(r.u64())
	v.Modified = Filetime(r.u64())
	v.Changed = Filetime(r.u64())
	v.Attributes = r.u32()
	r.skip(4)
	return v, r.exact()
}

// EncodeFileBasicInformation writes fields in MS-FSCC order.
func EncodeFileBasicInformation(v FileBasicInformation) ([]byte, error) {
	b := builder{}
	b.u64(uint64(v.Created))
	b.u64(uint64(v.Accessed))
	b.u64(uint64(v.Modified))
	b.u64(uint64(v.Changed))
	b.u32(v.Attributes)
	b.zero(4)
	return b.data, nil
}

// DecodeFileStandardInformation reads the MS-FSCC class buffer.
func DecodeFileStandardInformation(data []byte) (FileStandardInformation, error) {
	r := reader{data: data}
	var v FileStandardInformation
	v.AllocationSize = r.u64()
	v.EndOfFile = r.u64()
	v.Links = r.u32()
	v.DeletePending = r.boolean()
	v.Directory = r.boolean()
	r.skip(2)
	return v, r.exact()
}

// EncodeFileStandardInformation writes fields in MS-FSCC order.
func EncodeFileStandardInformation(v FileStandardInformation) ([]byte, error) {
	b := builder{}
	b.u64(v.AllocationSize)
	b.u64(v.EndOfFile)
	b.u32(v.Links)
	b.boolean(v.DeletePending)
	b.boolean(v.Directory)
	b.zero(2)
	return b.data, nil
}

// DecodeFileInternalInformation reads the MS-FSCC class buffer.
func DecodeFileInternalInformation(data []byte) (FileInternalInformation, error) {
	r := reader{data: data}
	var v FileInternalInformation
	v.Index = r.u64()
	return v, r.exact()
}

// EncodeFileInternalInformation writes fields in MS-FSCC order.
func EncodeFileInternalInformation(v FileInternalInformation) ([]byte, error) {
	b := builder{}
	b.u64(v.Index)
	return b.data, nil
}

// DecodeFileEAInformation reads the MS-FSCC class buffer.
func DecodeFileEAInformation(data []byte) (FileEAInformation, error) {
	r := reader{data: data}
	var v FileEAInformation
	v.Size = r.u32()
	return v, r.exact()
}

// EncodeFileEAInformation writes fields in MS-FSCC order.
func EncodeFileEAInformation(v FileEAInformation) ([]byte, error) {
	b := builder{}
	b.u32(v.Size)
	return b.data, nil
}

// DecodeFileAccessInformation reads the MS-FSCC class buffer.
func DecodeFileAccessInformation(data []byte) (FileAccessInformation, error) {
	r := reader{data: data}
	var v FileAccessInformation
	v.Access = r.u32()
	return v, r.exact()
}

// EncodeFileAccessInformation writes fields in MS-FSCC order.
func EncodeFileAccessInformation(v FileAccessInformation) ([]byte, error) {
	b := builder{}
	b.u32(v.Access)
	return b.data, nil
}

// DecodeFilePositionInformation reads the MS-FSCC class buffer.
func DecodeFilePositionInformation(data []byte) (FilePositionInformation, error) {
	r := reader{data: data}
	var v FilePositionInformation
	v.Offset = r.u64()
	return v, r.exact()
}

// EncodeFilePositionInformation writes fields in MS-FSCC order.
func EncodeFilePositionInformation(v FilePositionInformation) ([]byte, error) {
	b := builder{}
	b.u64(v.Offset)
	return b.data, nil
}

// DecodeFileModeInformation reads the MS-FSCC class buffer.
func DecodeFileModeInformation(data []byte) (FileModeInformation, error) {
	r := reader{data: data}
	var v FileModeInformation
	v.Mode = r.u32()
	return v, r.exact()
}

// EncodeFileModeInformation writes fields in MS-FSCC order.
func EncodeFileModeInformation(v FileModeInformation) ([]byte, error) {
	b := builder{}
	b.u32(v.Mode)
	return b.data, nil
}

// DecodeFileAlignmentInformation reads the MS-FSCC class buffer.
func DecodeFileAlignmentInformation(data []byte) (FileAlignmentInformation, error) {
	r := reader{data: data}
	var v FileAlignmentInformation
	v.Requirement = r.u32()
	return v, r.exact()
}

// EncodeFileAlignmentInformation writes fields in MS-FSCC order.
func EncodeFileAlignmentInformation(v FileAlignmentInformation) ([]byte, error) {
	b := builder{}
	b.u32(v.Requirement)
	return b.data, nil
}

// DecodeFileNetworkOpenInformation reads the MS-FSCC class buffer.
func DecodeFileNetworkOpenInformation(data []byte) (FileNetworkOpenInformation, error) {
	r := reader{data: data}
	var v FileNetworkOpenInformation
	v.Created = Filetime(r.u64())
	v.Accessed = Filetime(r.u64())
	v.Modified = Filetime(r.u64())
	v.Changed = Filetime(r.u64())
	v.AllocationSize = r.u64()
	v.EndOfFile = r.u64()
	v.Attributes = r.u32()
	r.skip(4)
	return v, r.exact()
}

// EncodeFileNetworkOpenInformation writes fields in MS-FSCC order.
func EncodeFileNetworkOpenInformation(v FileNetworkOpenInformation) ([]byte, error) {
	b := builder{}
	b.u64(uint64(v.Created))
	b.u64(uint64(v.Accessed))
	b.u64(uint64(v.Modified))
	b.u64(uint64(v.Changed))
	b.u64(v.AllocationSize)
	b.u64(v.EndOfFile)
	b.u32(v.Attributes)
	b.zero(4)
	return b.data, nil
}

// DecodeFileAttributeTagInformation reads the MS-FSCC class buffer.
func DecodeFileAttributeTagInformation(data []byte) (FileAttributeTagInformation, error) {
	r := reader{data: data}
	var v FileAttributeTagInformation
	v.Attributes = r.u32()
	v.Tag = r.u32()
	return v, r.exact()
}

// EncodeFileAttributeTagInformation writes fields in MS-FSCC order.
func EncodeFileAttributeTagInformation(v FileAttributeTagInformation) ([]byte, error) {
	b := builder{}
	b.u32(v.Attributes)
	b.u32(v.Tag)
	return b.data, nil
}

// DecodeFileIDInformation reads the MS-FSCC class buffer.
func DecodeFileIDInformation(data []byte) (FileIDInformation, error) {
	r := reader{data: data}
	var v FileIDInformation
	v.VolumeSerial = r.u64()
	v.ID = r.guid()
	return v, r.exact()
}

// EncodeFileIDInformation writes fields in MS-FSCC order.
func EncodeFileIDInformation(v FileIDInformation) ([]byte, error) {
	b := builder{}
	b.u64(v.VolumeSerial)
	b.guid(v.ID)
	return b.data, nil
}

// DecodeFileDispositionInformation reads the MS-FSCC class buffer.
func DecodeFileDispositionInformation(data []byte) (FileDispositionInformation, error) {
	r := reader{data: data}
	var v FileDispositionInformation
	v.DeletePending = r.boolean()
	return v, r.exact()
}

// EncodeFileDispositionInformation writes fields in MS-FSCC order.
func EncodeFileDispositionInformation(v FileDispositionInformation) ([]byte, error) {
	b := builder{}
	b.boolean(v.DeletePending)
	return b.data, nil
}

// DecodeFileEndOfFileInformation reads the MS-FSCC class buffer.
func DecodeFileEndOfFileInformation(data []byte) (FileEndOfFileInformation, error) {
	r := reader{data: data}
	var v FileEndOfFileInformation
	v.EndOfFile = r.u64()
	return v, r.exact()
}

// EncodeFileEndOfFileInformation writes fields in MS-FSCC order.
func EncodeFileEndOfFileInformation(v FileEndOfFileInformation) ([]byte, error) {
	b := builder{}
	b.u64(v.EndOfFile)
	return b.data, nil
}

// DecodeFileAllocationInformation reads the MS-FSCC class buffer.
func DecodeFileAllocationInformation(data []byte) (FileAllocationInformation, error) {
	r := reader{data: data}
	var v FileAllocationInformation
	v.AllocationSize = r.u64()
	return v, r.exact()
}

// EncodeFileAllocationInformation writes fields in MS-FSCC order.
func EncodeFileAllocationInformation(v FileAllocationInformation) ([]byte, error) {
	b := builder{}
	b.u64(v.AllocationSize)
	return b.data, nil
}

// DecodeFilesystemSizeInformation reads the MS-FSCC class buffer.
func DecodeFilesystemSizeInformation(data []byte) (FilesystemSizeInformation, error) {
	r := reader{data: data}
	var v FilesystemSizeInformation
	v.TotalUnits = r.u64()
	v.AvailableUnits = r.u64()
	v.SectorsPerUnit = r.u32()
	v.BytesPerSector = r.u32()
	return v, r.exact()
}

// EncodeFilesystemSizeInformation writes fields in MS-FSCC order.
func EncodeFilesystemSizeInformation(v FilesystemSizeInformation) ([]byte, error) {
	b := builder{}
	b.u64(v.TotalUnits)
	b.u64(v.AvailableUnits)
	b.u32(v.SectorsPerUnit)
	b.u32(v.BytesPerSector)
	return b.data, nil
}

// DecodeFilesystemFullSizeInformation reads the MS-FSCC class buffer.
func DecodeFilesystemFullSizeInformation(data []byte) (FilesystemFullSizeInformation, error) {
	r := reader{data: data}
	var v FilesystemFullSizeInformation
	v.TotalUnits = r.u64()
	v.CallerAvailableUnits = r.u64()
	v.ActualAvailableUnits = r.u64()
	v.SectorsPerUnit = r.u32()
	v.BytesPerSector = r.u32()
	return v, r.exact()
}

// EncodeFilesystemFullSizeInformation writes fields in MS-FSCC order.
func EncodeFilesystemFullSizeInformation(v FilesystemFullSizeInformation) ([]byte, error) {
	b := builder{}
	b.u64(v.TotalUnits)
	b.u64(v.CallerAvailableUnits)
	b.u64(v.ActualAvailableUnits)
	b.u32(v.SectorsPerUnit)
	b.u32(v.BytesPerSector)
	return b.data, nil
}

// DecodeFilesystemDeviceInformation reads the MS-FSCC class buffer.
func DecodeFilesystemDeviceInformation(data []byte) (FilesystemDeviceInformation, error) {
	r := reader{data: data}
	var v FilesystemDeviceInformation
	v.Type = r.u32()
	v.Characteristics = r.u32()
	return v, r.exact()
}

// EncodeFilesystemDeviceInformation writes fields in MS-FSCC order.
func EncodeFilesystemDeviceInformation(v FilesystemDeviceInformation) ([]byte, error) {
	b := builder{}
	b.u32(v.Type)
	b.u32(v.Characteristics)
	return b.data, nil
}
