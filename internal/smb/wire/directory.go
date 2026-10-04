package wire

func readDirectoryMetadata(r *reader) (DirectoryMetadata, uint32) {
	var v DirectoryMetadata
	v.FileIndex = r.u32()
	v.Basic.Created = Filetime(r.u64())
	v.Basic.Accessed = Filetime(r.u64())
	v.Basic.Modified = Filetime(r.u64())
	v.Basic.Changed = Filetime(r.u64())
	v.EndOfFile = r.u64()
	v.AllocationSize = r.u64()
	v.Basic.Attributes = r.u32()
	nameLength := r.u32()
	v.EASize = r.u32()
	return v, nameLength
}
func writeDirectoryMetadata(b *builder, v DirectoryMetadata, nameLength uint32) {
	b.u32(v.FileIndex)
	b.u64(uint64(v.Basic.Created))
	b.u64(uint64(v.Basic.Accessed))
	b.u64(uint64(v.Basic.Modified))
	b.u64(uint64(v.Basic.Changed))
	b.u64(v.EndOfFile)
	b.u64(v.AllocationSize)
	b.u32(v.Basic.Attributes)
	b.u32(nameLength)
	b.u32(v.EASize)
}

// DecodeDirectoryIDBothEntries validates names and every class 37 link.
func DecodeDirectoryIDBothEntries(data []byte) ([]DirectoryIDBothEntry, error) {
	var entries []DirectoryIDBothEntry
	for len(data) > 0 {
		r, next, err := linkedMember(data, 104)
		if err != nil {
			return nil, err
		}
		metadata, n := readDirectoryMetadata(r)
		shortLength := r.u8()
		r.skip(1)
		short := r.take(24)
		r.skip(2)
		metadata.FileID = r.u64()
		if shortLength > 24 || shortLength%2 != 0 {
			return nil, errMalformed
		}
		entry := DirectoryIDBothEntry{Metadata: metadata, ShortName: r.text(short[:int(shortLength)])}
		entry.Name = r.text(r.region(104, uint64(n), 104, 2))
		if r.err != nil {
			return nil, r.err
		}
		entries = append(entries, entry)
		if next == 0 {
			break
		}
		data = data[int(next):]
	}
	return entries, nil
}

// EncodeDirectoryIDBothEntries writes class 37, including the short-name slot.
func EncodeDirectoryIDBothEntries(entries []DirectoryIDBothEntry) ([]byte, error) {
	b := builder{}
	for i, e := range entries {
		name, err := encodeUTF16(e.Name)
		if err != nil {
			return nil, err
		}
		short, err := encodeUTF16(e.ShortName)
		if err != nil {
			return nil, err
		}
		if len(short) > 24 || !size32(111+len(name)) {
			return nil, errMalformed
		}
		member := builder{}
		n := 104 + len(name)
		if i < len(entries)-1 {
			n += (8 - n%8) % 8
			member.u32(uint32(n))
		} else {
			member.u32(0)
		}
		writeDirectoryMetadata(&member, e.Metadata, uint32(len(name)))
		member.u8(uint8(len(short)))
		member.u8(0)
		member.bytes(short)
		member.zero(24 - len(short))
		member.u16(0)
		member.u64(e.Metadata.FileID)
		member.bytes(name)
		if i < len(entries)-1 {
			member.align(8)
		}
		b.bytes(member.data)
	}
	return b.data, nil
}

// DecodeDirectoryIDFullEntries validates names and every class 38 link.
func DecodeDirectoryIDFullEntries(data []byte) ([]DirectoryIDFullEntry, error) {
	var entries []DirectoryIDFullEntry
	for len(data) > 0 {
		r, next, err := linkedMember(data, 80)
		if err != nil {
			return nil, err
		}
		metadata, n := readDirectoryMetadata(r)
		r.skip(4)
		metadata.FileID = r.u64()
		entry := DirectoryIDFullEntry{Metadata: metadata, Name: r.text(r.region(80, uint64(n), 80, 2))}
		if r.err != nil {
			return nil, r.err
		}
		entries = append(entries, entry)
		if next == 0 {
			break
		}
		data = data[int(next):]
	}
	return entries, nil
}

// EncodeDirectoryIDFullEntries writes an aligned class 38 list.
func EncodeDirectoryIDFullEntries(entries []DirectoryIDFullEntry) ([]byte, error) {
	b := builder{}
	for i, e := range entries {
		name, err := encodeUTF16(e.Name)
		if err != nil {
			return nil, err
		}
		if !size32(87 + len(name)) {
			return nil, errMalformed
		}
		member := builder{}
		n := 80 + len(name)
		if i < len(entries)-1 {
			n += (8 - n%8) % 8
			member.u32(uint32(n))
		} else {
			member.u32(0)
		}
		writeDirectoryMetadata(&member, e.Metadata, uint32(len(name)))
		member.u32(0)
		member.u64(e.Metadata.FileID)
		member.bytes(name)
		if i < len(entries)-1 {
			member.align(8)
		}
		b.bytes(member.data)
	}
	return b.data, nil
}
