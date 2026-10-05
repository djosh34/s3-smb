package state

// TraceCounts is temporary research logging for #528. It returns the number
// of opens and of distinct inodes they hold.
func (table *Table) TraceCounts() (opens, files int) {
	table.mu.Lock()
	defer table.mu.Unlock()
	inodes := make(map[uint64]struct{}, len(table.opens))
	for _, open := range table.opens {
		inodes[uint64(open.Object.Inode)] = struct{}{}
	}
	return len(table.opens), len(inodes)
}
