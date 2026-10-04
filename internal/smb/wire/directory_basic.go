package wire

// DecodeDirectoryEntries validates the linked class 1 list.
func DecodeDirectoryEntries(data []byte) ([]DirectoryEntry, error) {
	return decodeBasicDirectoryEntries(data, ClassDirectory)
}

// EncodeDirectoryEntries writes class 1 entries.
func EncodeDirectoryEntries(entries []DirectoryEntry) ([]byte, error) {
	return encodeBasicDirectoryEntries(entries, ClassDirectory)
}

// DecodeDirectoryFullEntries validates the linked class 2 list.
func DecodeDirectoryFullEntries(data []byte) ([]DirectoryEntry, error) {
	return decodeBasicDirectoryEntries(data, ClassDirectoryFull)
}

// EncodeDirectoryFullEntries writes class 2 entries.
func EncodeDirectoryFullEntries(entries []DirectoryEntry) ([]byte, error) {
	return encodeBasicDirectoryEntries(entries, ClassDirectoryFull)
}

// DecodeDirectoryBothEntries validates the linked class 3 list.
func DecodeDirectoryBothEntries(data []byte) ([]DirectoryEntry, error) {
	return decodeBasicDirectoryEntries(data, ClassDirectoryBoth)
}

// EncodeDirectoryBothEntries writes class 3 entries.
func EncodeDirectoryBothEntries(entries []DirectoryEntry) ([]byte, error) {
	return encodeBasicDirectoryEntries(entries, ClassDirectoryBoth)
}

// DecodeDirectoryNamesEntries validates the linked class 12 list.
func DecodeDirectoryNamesEntries(data []byte) ([]DirectoryEntry, error) {
	return decodeBasicDirectoryEntries(data, ClassDirectoryNames)
}

// EncodeDirectoryNamesEntries writes class 12 entries.
func EncodeDirectoryNamesEntries(entries []DirectoryEntry) ([]byte, error) {
	return encodeBasicDirectoryEntries(entries, ClassDirectoryNames)
}

func basicDirectorySize(class DirectoryInfoClass) uint32 {
	switch uint8(class) {
	case uint8(ClassDirectory):
		return 64
	case uint8(ClassDirectoryFull):
		return 68
	case uint8(ClassDirectoryBoth):
		return 94
	default: // FileNamesInformation.
		return 12
	}
}

func decodeBasicDirectoryEntries(data []byte, class DirectoryInfoClass) ([]DirectoryEntry, error) {
	var entries []DirectoryEntry
	fixed := basicDirectorySize(class)
	for len(data) > 0 {
		r, next, err := linkedMember(data, fixed)
		if err != nil {
			return nil, err
		}
		var entry DirectoryEntry
		var length uint32
		if class == ClassDirectoryNames {
			entry.Metadata.FileIndex = r.u32()
			length = r.u32()
		} else {
			entry.Metadata, length = readDirectoryBase(r)
			if class != ClassDirectory {
				entry.Metadata.EASize = r.u32()
			}
			if class == ClassDirectoryBoth {
				n := r.u8()
				r.skip(1)
				short := r.take(24)
				if n > 24 || n%2 != 0 {
					return nil, errMalformed
				}
				entry.ShortName = r.text(short[:int(n)])
			}
		}
		entry.Name = r.text(r.region(uint64(fixed), uint64(length), uint64(fixed), 2))
		if next == 0 {
			r.paddingAfter(uint64(fixed) + uint64(length))
		}
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

func encodeBasicDirectoryEntries(entries []DirectoryEntry, class DirectoryInfoClass) ([]byte, error) {
	b := builder{}
	fixed := int(basicDirectorySize(class))
	for i, entry := range entries {
		name, err := encodeUTF16(entry.Name)
		if err != nil {
			return nil, err
		}
		if !size32(fixed + len(name) + 7) {
			return nil, errMalformed
		}
		member := builder{}
		n := fixed + len(name)
		if i < len(entries)-1 {
			member.length32(n + (8-n%8)%8)
		} else {
			member.u32(0)
		}
		if class == ClassDirectoryNames {
			member.u32(entry.Metadata.FileIndex)
			member.length32(len(name))
		} else {
			writeDirectoryBase(&member, entry.Metadata, len(name))
			if class != ClassDirectory {
				member.u32(entry.Metadata.EASize)
			}
			if class == ClassDirectoryBoth {
				short, shortErr := encodeUTF16(entry.ShortName)
				if shortErr != nil {
					return nil, shortErr
				}
				if len(short) > 24 {
					return nil, errMalformed
				}
				member.length8(len(short))
				member.u8(0)
				member.bytes(short)
				member.zero(24 - len(short))
			}
		}
		member.bytes(name)
		if i < len(entries)-1 {
			member.align8()
		}
		data, err := member.finish()
		if err != nil {
			return nil, err
		}
		b.bytes(data)
	}
	return b.finish()
}
