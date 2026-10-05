package server

import (
	"context"
	"strings"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func setRenameInfo(ctx context.Context, request RequestContext, open state.Open, buffer []byte) smb.Status {
	info, err := wire.DecodeFileRenameInformation(buffer)
	if err != nil {
		return smb.StatusInvalidParameter
	}
	if info.RootDirectory != 0 {
		return smb.StatusNotSupported
	}
	if open.Object.Stream != "" {
		return smb.StatusNotSupported
	}
	if open.GrantedAccess&0x00010000 == 0 {
		return smb.StatusAccessDenied
	}
	// A leading SMB separator names a destination relative to the share root.
	path := strings.TrimPrefix(info.Name, "\\")
	for range nameTries {
		if err := ctx.Err(); err != nil {
			return smb.StatusFromError(err)
		}
		sourcePath, source, err := renameSource(ctx, request, open.Object.Inode)
		if err != nil {
			return smb.StatusFromError(err)
		}
		destination, err := request.Storage.Lookup(ctx, path)
		if err != nil {
			return smb.StatusFromError(err)
		}
		if destination.Name.Stream != "" {
			return smb.StatusNotSupported
		}
		if source.Name.Parent == 0 || destination.Name.Parent == 0 {
			return smb.StatusAccessDenied
		}
		unlock, err := lockParents(ctx, request, source.Name.Parent, destination.Name.Parent)
		if err != nil {
			return smb.StatusFromError(err)
		}
		status, retry := renameUnderGuard(ctx, request, open, sourcePath, path, source, destination, info.ReplaceIfExists)
		unlock()
		if !retry {
			return status
		}
	}
	return smb.StatusFromError(smb.ErrIdentityChanged)
}

func renameSource(ctx context.Context, request RequestContext, inode smb.Inode) (string, smb.Resolved, error) {
	path, err := request.Storage.PathOf(ctx, inode)
	if err != nil {
		return "", smb.Resolved{}, err
	}
	resolved, err := request.Storage.Lookup(ctx, path)
	return path, resolved, err
}

// Only the lookups under both parent guards select the mutation identities.
func renameUnderGuard(ctx context.Context, request RequestContext, open state.Open, sourcePath, destinationPath string, discoveredSource, discoveredDestination smb.Resolved, replace bool) (smb.Status, bool) {
	source, err := request.Storage.Lookup(ctx, sourcePath)
	if err != nil {
		return smb.StatusFromError(err), false
	}
	destination, err := request.Storage.Lookup(ctx, destinationPath)
	if err != nil {
		return smb.StatusFromError(err), false
	}
	if source.Name.Parent != discoveredSource.Name.Parent || destination.Name.Parent != discoveredDestination.Name.Parent || !source.Exists || source.Object != open.Object {
		return smb.StatusSuccess, true
	}
	if destination.Exists {
		if !replace {
			return smb.StatusObjectNameCollision, false
		}
		if request.Opens.InodeOpen(destination.Object.Inode) {
			return smb.StatusAccessDenied, false
		}
	}
	// Reserve enforces DELETE sharing in both directions when opens are added.
	// A source open granted DELETE cannot coexist with a deny-delete open.
	err = request.Storage.Rename(ctx, smb.RenameRequest{
		Source: source.Name, Destination: destination.Name,
		SourceInode: open.Object.Inode, DestinationInode: destination.Object.Inode,
		Replace: replace,
	})
	return smb.StatusFromError(err), false
}

func setDispositionInfo(ctx context.Context, request RequestContext, open state.Open, buffer []byte) smb.Status {
	info, err := wire.DecodeFileDispositionInformation(buffer)
	if err != nil {
		return smb.StatusInvalidParameter
	}
	if open.GrantedAccess&0x00010000 == 0 {
		return smb.StatusAccessDenied
	}
	if !info.DeletePending {
		return request.Opens.SetDelete(open.ID, request.Binding(), smb.Name{}, false)
	}
	for range nameTries {
		if err := ctx.Err(); err != nil {
			return smb.StatusFromError(err)
		}
		path, discovered, err := renameSource(ctx, request, open.Object.Inode)
		if err != nil {
			return smb.StatusFromError(err)
		}
		if discovered.Name.Parent == 0 {
			return smb.StatusAccessDenied
		}
		// The inode guard also keeps child CREATE out of an emptiness check.
		unlock, err := lockParents(ctx, request, discovered.Name.Parent, open.Object.Inode)
		if err != nil {
			return smb.StatusFromError(err)
		}
		status, retry := dispositionUnderGuard(ctx, request, open, path, discovered)
		unlock()
		if !retry {
			return status
		}
	}
	return smb.StatusFromError(smb.ErrIdentityChanged)
}

func dispositionUnderGuard(ctx context.Context, request RequestContext, open state.Open, path string, discovered smb.Resolved) (smb.Status, bool) {
	resolved, err := request.Storage.Lookup(ctx, path)
	if err != nil {
		return smb.StatusFromError(err), false
	}
	if resolved.Name.Parent != discovered.Name.Parent || !resolved.Exists || resolved.Object.Inode != open.Object.Inode {
		return smb.StatusSuccess, true
	}
	if open.Object.Stream == "" && resolved.Attr.Kind == smb.KindDirectory {
		entries, err := request.Storage.ReadDir(ctx, open.Object.Inode, 0, 1)
		if err != nil {
			return smb.StatusFromError(err), false
		}
		if len(entries) != 0 {
			return smb.StatusDirectoryNotEmpty, false
		}
	}
	name := resolved.Name
	name.Stream = open.Object.Stream
	return request.Opens.SetDelete(open.ID, request.Binding(), name, true), false
}
