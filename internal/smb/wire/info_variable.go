package wire

// DecodeFileNameInformation reads a length-prefixed UTF-16 name.
func DecodeFileNameInformation(data []byte) (FileNameInformation, error) {
	r := reader{data: data}
	n := r.u32()
	v := FileNameInformation{Name: r.text(r.region(4, uint64(n), 4, 2))}
	if uint64(n)+4 != uint64(len(data)) {
		r.err = errMalformed
	}
	return v, r.err
}

// EncodeFileNameInformation writes a length-prefixed UTF-16 name.
func EncodeFileNameInformation(v FileNameInformation) ([]byte, error) {
	name, err := encodeUTF16(v.Name)
	if err != nil {
		return nil, err
	}
	b := builder{}
	b.length32(len(name))
	b.bytes(name)
	return b.finish()
}

// DecodeFileAllInformation reads the classes in MS-FSCC order.
func DecodeFileAllInformation(data []byte) (FileAllInformation, error) {
	r := reader{data: data}
	var v FileAllInformation
	var err error
	v.Basic, err = DecodeFileBasicInformation(r.take(40))
	if err != nil {
		return v, err
	}
	v.Standard, err = DecodeFileStandardInformation(r.take(24))
	if err != nil {
		return v, err
	}
	v.Internal, err = DecodeFileInternalInformation(r.take(8))
	if err != nil {
		return v, err
	}
	v.EA, err = DecodeFileEAInformation(r.take(4))
	if err != nil {
		return v, err
	}
	v.Access, err = DecodeFileAccessInformation(r.take(4))
	if err != nil {
		return v, err
	}
	v.Position, err = DecodeFilePositionInformation(r.take(8))
	if err != nil {
		return v, err
	}
	v.Mode, err = DecodeFileModeInformation(r.take(4))
	if err != nil {
		return v, err
	}
	v.Alignment, err = DecodeFileAlignmentInformation(r.take(4))
	if err != nil {
		return v, err
	}
	v.Name, err = DecodeFileNameInformation(r.take(len(data) - r.pos))
	return v, err
}

// EncodeFileAllInformation writes each class in its declared wire order.
func EncodeFileAllInformation(v FileAllInformation) ([]byte, error) {
	b := builder{}
	b.encoded(EncodeFileBasicInformation(v.Basic))
	b.encoded(EncodeFileStandardInformation(v.Standard))
	b.encoded(EncodeFileInternalInformation(v.Internal))
	b.encoded(EncodeFileEAInformation(v.EA))
	b.encoded(EncodeFileAccessInformation(v.Access))
	b.encoded(EncodeFilePositionInformation(v.Position))
	b.encoded(EncodeFileModeInformation(v.Mode))
	b.encoded(EncodeFileAlignmentInformation(v.Alignment))
	b.encoded(EncodeFileNameInformation(v.Name))
	return b.finish()
}

// DecodeFileRenameInformation reads the declared name and ignores extra bytes.
func DecodeFileRenameInformation(data []byte) (FileRenameInformation, error) {
	r := reader{data: data}
	var v FileRenameInformation
	v.ReplaceIfExists = r.boolean()
	r.skip(7)
	v.RootDirectory = r.u64()
	n := r.u32()
	v.Name = r.text(r.region(20, uint64(n), 20, 2))
	return v, r.err
}

// EncodeFileRenameInformation writes the rename class buffer.
func EncodeFileRenameInformation(v FileRenameInformation) ([]byte, error) {
	name, err := encodeUTF16(v.Name)
	if err != nil {
		return nil, err
	}
	b := builder{}
	b.boolean(v.ReplaceIfExists)
	b.zero(7)
	b.u64(v.RootDirectory)
	b.length32(len(name))
	b.bytes(name)
	return b.finish()
}

// DecodeFilesystemVolumeInformation reads the volume label and fixed fields.
func DecodeFilesystemVolumeInformation(data []byte) (FilesystemVolumeInformation, error) {
	r := reader{data: data}
	var v FilesystemVolumeInformation
	v.Created = Filetime(r.u64())
	v.Serial = r.u32()
	n := r.u32()
	v.SupportsObjects = r.boolean()
	r.skip(1)
	v.Label = r.text(r.region(18, uint64(n), 18, 2))
	if uint64(n)+18 != uint64(len(data)) {
		r.err = errMalformed
	}
	return v, r.err
}

// EncodeFilesystemVolumeInformation writes the volume class buffer.
func EncodeFilesystemVolumeInformation(v FilesystemVolumeInformation) ([]byte, error) {
	name, err := encodeUTF16(v.Label)
	if err != nil {
		return nil, err
	}
	b := builder{}
	b.u64(uint64(v.Created))
	b.u32(v.Serial)
	b.length32(len(name))
	b.boolean(v.SupportsObjects)
	b.u8(0)
	b.bytes(name)
	return b.finish()
}

// DecodeFilesystemAttributeInformation reads the filesystem name and mask.
func DecodeFilesystemAttributeInformation(data []byte) (FilesystemAttributeInformation, error) {
	r := reader{data: data}
	var v FilesystemAttributeInformation
	v.Attributes = r.u32()
	v.MaxComponentLength = r.i32()
	n := r.u32()
	v.Name = r.text(r.region(12, uint64(n), 12, 2))
	if uint64(n)+12 != uint64(len(data)) {
		r.err = errMalformed
	}
	return v, r.err
}

// EncodeFilesystemAttributeInformation writes the filesystem class buffer.
func EncodeFilesystemAttributeInformation(v FilesystemAttributeInformation) ([]byte, error) {
	name, err := encodeUTF16(v.Name)
	if err != nil {
		return nil, err
	}
	b := builder{}
	b.u32(v.Attributes)
	b.i32(v.MaxComponentLength)
	b.length32(len(name))
	b.bytes(name)
	return b.finish()
}

// linkedMember bounds one entry and validates the next link before decoding it.
func linkedMember(data []byte, minimum uint32) (*reader, uint32, error) {
	r := &reader{data: data}
	next := r.u32()
	if r.err != nil || uint64(len(data)) < uint64(minimum) {
		return r, 0, errMalformed
	}
	if next != 0 {
		if next%8 != 0 || uint64(next) < uint64(minimum) || uint64(next)+uint64(minimum) > uint64(len(data)) {
			return r, 0, errMalformed
		}
		r.data = data[:int(next)]
	}
	return r, next, nil
}
