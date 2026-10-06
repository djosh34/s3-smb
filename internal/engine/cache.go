// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"slices"
	"sync"
)

// readCache keeps the chunks read last from S3, whole, so the many small
// reads that mounting a disk image makes cost one GET per chunk, not one per
// read. Chunk names are never reused, so an entry never goes stale.
type readCache struct {
	chunks []cachedChunk // most recently used first
	mu     sync.Mutex
}

type cachedChunk struct {
	name string
	data []byte
}

// get returns the chunk's bytes, which the caller must not change, or nil.
func (c *readCache) get(name string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := slices.IndexFunc(c.chunks, func(chunk cachedChunk) bool { return chunk.name == name })
	if i < 0 {
		return nil
	}
	chunk := c.chunks[i]
	copy(c.chunks[1:i+1], c.chunks[:i])
	c.chunks[0] = chunk
	return chunk.data
}

// add keeps data as the most recently used chunk, and at most limit chunks.
func (c *readCache) add(name string, data []byte, limit int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slices.ContainsFunc(c.chunks, func(chunk cachedChunk) bool { return chunk.name == name }) {
		return
	}
	c.chunks = slices.Insert(c.chunks, 0, cachedChunk{name: name, data: data})
	if len(c.chunks) > limit {
		c.chunks = slices.Delete(c.chunks, limit, len(c.chunks))
	}
}
