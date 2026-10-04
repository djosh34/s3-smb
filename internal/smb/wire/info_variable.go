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
	if !size32(len(name)) {
		return nil, errMalformed
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
	parts := []func() ([]byte, error){
		func() ([]byte, error) { return EncodeFileBasicInformation(v.Basic) },
		func() ([]byte, error) { return EncodeFileStandardInformation(v.Standard) },
		func() ([]byte, error) { return EncodeFileInternalInformation(v.Internal) },
		func() ([]byte, error) { return EncodeFileEAInformation(v.EA) },
		func() ([]byte, error) { return EncodeFileAccessInformation(v.Access) },
		func() ([]byte, error) { return EncodeFilePositionInformation(v.Position) },
		func() ([]byte, error) { return EncodeFileModeInformation(v.Mode) },
		func() ([]byte, error) { return EncodeFileAlignmentInformation(v.Alignment) },
		func() ([]byte, error) { return EncodeFileNameInformation(v.Name) },
	}
	for _, part := range parts {
		data, err := part()
		if err != nil {
			return nil, err
		}
		b.bytes(data)
	}
	return b.data, nil
}

// DecodeFileRenameInformation preserves the destination and root identity.
func DecodeFileRenameInformation(data []byte) (FileRenameInformation, error) {
	r := reader{data: data}
	var v FileRenameInformation
	v.ReplaceIfExists = r.boolean()
	r.skip(7)
	v.RootDirectory = r.u64()
	n := r.u32()
	v.Name = r.text(r.region(20, uint64(n), 20, 2))
	if uint64(n)+20 != uint64(len(data)) {
		r.err = errMalformed
	}
	return v, r.err
}

// EncodeFileRenameInformation writes the rename class buffer.
func EncodeFileRenameInformation(v FileRenameInformation) ([]byte, error) {
	name, err := encodeUTF16(v.Name)
	if err != nil {
		return nil, err
	}
	if !size32(len(name)) {
		return nil, errMalformed
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
	if !size32(len(name)) {
		return nil, errMalformed
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
	if !size32(len(name)) {
		return nil, errMalformed
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

// DecodeFileStreamInformation validates the entire linked stream list.
func DecodeFileStreamInformation(data []byte) (FileStreamInformation, error) {
	var v FileStreamInformation
	for len(data) > 0 {
		r, next, err := linkedMember(data, 24)
		if err != nil {
			return FileStreamInformation{}, err
		}
		n := r.u32()
		entry := FileStreamEntry{Size: r.u64(), AllocationSize: r.u64()}
		entry.Name = r.text(r.region(24, uint64(n), 24, 2))
		if r.err != nil {
			return FileStreamInformation{}, r.err
		}
		v.Entries = append(v.Entries, entry)
		if next == 0 {
			break
		}
		data = data[int(next):]
	}
	return v, nil
}

// EncodeFileStreamInformation writes an aligned linked stream list.
func EncodeFileStreamInformation(v FileStreamInformation) ([]byte, error) {
	b := builder{}
	for i, e := range v.Entries {
		name, err := encodeUTF16(e.Name)
		if err != nil {
			return nil, err
		}
		if !size32(len(name) + 31) {
			return nil, errMalformed
		}
		member := builder{}
		n := 24 + len(name)
		if i < len(v.Entries)-1 {
			n += (8 - n%8) % 8
			member.length32(n)
		} else {
			member.u32(0)
		}
		member.length32(len(name))
		member.u64(e.Size)
		member.u64(e.AllocationSize)
		member.bytes(name)
		if i < len(v.Entries)-1 {
			member.align(8)
		}
		data, err := member.finish()
		if err != nil {
			return nil, err
		}
		b.bytes(data)
	}
	return b.finish()
}
