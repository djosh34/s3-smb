package wire

// DecodeSID reads the authority and the exact subauthority vector.
func DecodeSID(data []byte) (SID, error) {
	r := reader{data: data}
	var v SID
	v.Revision = r.u8()
	count := r.u8()
	copy(v.Authority[:], r.take(6))
	if r.err != nil || v.Revision != 1 || count > 15 {
		return v, errMalformed
	}
	if count > 0 {
		v.SubAuthorities = make([]uint32, count)
	}
	for i := range v.SubAuthorities {
		v.SubAuthorities[i] = r.u32()
	}
	return v, r.exact()
}

// EncodeSID writes a revision-one security identifier.
func EncodeSID(v SID) ([]byte, error) {
	if v.Revision != 1 || len(v.SubAuthorities) > 15 {
		return nil, errMalformed
	}
	b := builder{}
	b.u8(v.Revision)
	b.length8(len(v.SubAuthorities))
	b.bytes(v.Authority[:])
	for _, a := range v.SubAuthorities {
		b.u32(a)
	}
	return b.finish()
}

// DecodeACL supports only the declared simple access and audit ACEs.
func DecodeACL(data []byte) (ACL, error) {
	r := reader{data: data}
	var v ACL
	v.Revision = r.u8()
	r.skip(1)
	size := r.u16()
	count := r.u16()
	r.skip(2)
	if r.err != nil || v.Revision != 2 && v.Revision != 4 || int(size) != len(data) || size%4 != 0 || int(count) > (len(data)-8)/16 {
		return v, errMalformed
	}
	for i := uint16(0); i < count; i++ {
		start := r.pos
		typ := ACEType(r.u8())
		flags := r.u8()
		n := r.u16()
		mask := r.u32()
		if r.err != nil || typ > ACEAudit || n < 16 || n%4 != 0 || int(n) > len(data)-start {
			return ACL{}, errMalformed
		}
		trustee, err := DecodeSID(r.take(int(n) - 8))
		if err != nil {
			return ACL{}, err
		}
		v.Entries = append(v.Entries, ACE{Type: typ, Flags: flags, Mask: mask, Trustee: trustee})
	}
	// AclSize can include unused capacity after the last ACE.
	return v, r.err
}

// EncodeACL writes simple ACEs and calculates ACL and ACE sizes.
func EncodeACL(v ACL) ([]byte, error) {
	if v.Revision != 2 && v.Revision != 4 || !size16(len(v.Entries)) {
		return nil, errMalformed
	}
	entries := builder{}
	for _, e := range v.Entries {
		if e.Type > ACEAudit {
			return nil, errMalformed
		}
		sid, err := EncodeSID(e.Trustee)
		if err != nil {
			return nil, err
		}
		entries.u8(uint8(e.Type))
		entries.u8(e.Flags)
		entries.length16(8 + len(sid))
		entries.u32(e.Mask)
		entries.bytes(sid)
		if !size16(8 + len(entries.data)) {
			return nil, errMalformed
		}
	}
	b := builder{}
	b.u8(v.Revision)
	b.u8(0)
	b.length16(8 + len(entries.data))
	b.length16(len(v.Entries))
	b.u16(0)
	data, err := entries.finish()
	if err != nil {
		return nil, err
	}
	b.bytes(data)
	return b.finish()
}

func (r *reader) descriptorSID(offset uint32) (*SID, error) {
	// Read the count without trusting the descriptor-relative offset.
	probe := reader{data: r.data}
	header := probe.region(uint64(offset), 8, 20, 4)
	if probe.err != nil {
		return nil, probe.err
	}
	count := header[1]
	data := r.region(uint64(offset), 8+4*uint64(count), 20, 4)
	if r.err != nil {
		return nil, r.err
	}
	v, err := DecodeSID(data)
	if err != nil {
		return nil, err
	}
	return &v, nil
}
func (r *reader) descriptorACL(offset uint32) (*ACL, error) {
	probe := reader{data: r.data}
	header := probe.region(uint64(offset), 8, 20, 4)
	if probe.err != nil {
		return nil, probe.err
	}
	h := reader{data: header}
	h.skip(2)
	n := h.u16()
	data := r.region(uint64(offset), uint64(n), 20, 4)
	if r.err != nil {
		return nil, r.err
	}
	v, err := DecodeACL(data)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// DecodeSecurityDescriptor preserves absent, null and empty ACLs.
func DecodeSecurityDescriptor(data []byte) (SecurityDescriptor, error) {
	r := reader{data: data}
	var v SecurityDescriptor
	v.Revision = r.u8()
	r.skip(1)
	v.Control = r.u16()
	owner := r.u32()
	group := r.u32()
	sacl := r.u32()
	dacl := r.u32()
	if r.err != nil || v.Revision != 1 || v.Control&DescriptorSelfRelative == 0 || v.Control&SACLPresent == 0 && sacl != 0 || v.Control&DACLPresent == 0 && dacl != 0 {
		return v, errMalformed
	}
	var err error
	if owner != 0 {
		v.Owner, err = r.descriptorSID(owner)
		if err != nil {
			return SecurityDescriptor{}, err
		}
	}
	if group != 0 {
		v.Group, err = r.descriptorSID(group)
		if err != nil {
			return SecurityDescriptor{}, err
		}
	}
	if sacl != 0 {
		v.SACL, err = r.descriptorACL(sacl)
		if err != nil {
			return SecurityDescriptor{}, err
		}
	}
	if dacl != 0 {
		v.DACL, err = r.descriptorACL(dacl)
		if err != nil {
			return SecurityDescriptor{}, err
		}
	}
	return v, r.err
}

// EncodeSecurityDescriptor writes a self-relative descriptor with owned offsets.
func EncodeSecurityDescriptor(v SecurityDescriptor) ([]byte, error) {
	if v.Revision != 1 || v.Control&DescriptorSelfRelative == 0 || v.Control&SACLPresent == 0 && v.SACL != nil || v.Control&DACLPresent == 0 && v.DACL != nil {
		return nil, errMalformed
	}
	var parts [4][]byte
	var err error
	if v.Owner != nil {
		parts[0], err = EncodeSID(*v.Owner)
		if err != nil {
			return nil, err
		}
	}
	if v.Group != nil {
		parts[1], err = EncodeSID(*v.Group)
		if err != nil {
			return nil, err
		}
	}
	if v.SACL != nil {
		parts[2], err = EncodeACL(*v.SACL)
		if err != nil {
			return nil, err
		}
	}
	if v.DACL != nil {
		parts[3], err = EncodeACL(*v.DACL)
		if err != nil {
			return nil, err
		}
	}
	b := builder{}
	b.u8(v.Revision)
	b.u8(0)
	b.u16(v.Control)
	offset := 20
	for _, p := range parts {
		if len(p) == 0 {
			b.u32(0)
		} else {
			b.length32(offset)
			offset += len(p)
		}
	}
	for _, p := range parts {
		b.align(4)
		b.bytes(p)
	}
	return b.finish()
}
