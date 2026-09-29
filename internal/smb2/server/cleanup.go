package smb2

import (
	"context"
	"errors"

	. "github.com/djosh34/s3-smb/internal/smb2/internal/smb2"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

// closeHandle always releases the handle, even when durability or unlink fails.
func (t *fileTree) closeHandle(id *FileId, open *Open) error {
	h := vfs.VfsHandle(id.HandleId())
	err := t.fs.Flush(h)
	err = errors.Join(err, t.applyPosixDeleteOnClose(id, open))
	if t.conn.serverCtx.closeOpen(open) {
		err = errors.Join(err, t.fs.Unlink(h))
	}
	return errors.Join(err, t.fs.Close(h))
}

func (c *conn) closeTreeHandles(tree *treeConn) error {
	c.ioWG.Wait()
	c.serverCtx.lock.Lock()
	var opens []*Open
	for _, o := range c.serverCtx.opens {
		if o.session != nil && o.session.conn == c && (tree == nil || o.tree == tree) {
			opens = append(opens, o)
		}
	}
	c.serverCtx.lock.Unlock()
	var err error
	for _, o := range opens {
		if t, ok := c.treeMapById[o.tree.treeId].(*fileTree); ok {
			id := &FileId{}
			id.SetHandleId(o.fileId)
			id.SetNodeId(o.durableFileId)
			err = errors.Join(err, t.closeHandle(id, o))
		}
	}
	return err
}

// ShutdownContext waits for native connection work and handle cleanup. It never
// closes the VFS itself; the owner must do that only after successful draining.
func (d *Server) ShutdownContext(ctx context.Context) error {
	d.Shutdown()
	done := make(chan struct{})
	go func() { d.connWG.Wait(); close(done) }()
	select {
	case <-done:
		d.lock.Lock()
		defer d.lock.Unlock()
		return d.cleanupErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
