package server

// Temporary research logging for #528. One "smb trace" line per request.

import (
	"context"
	"os"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

var traceCommands = map[wire.Command]string{
	wire.Negotiate: "NEGOTIATE", wire.SessionSetup: "SESSION_SETUP", wire.Logoff: "LOGOFF",
	wire.TreeConnect: "TREE_CONNECT", wire.TreeDisconnect: "TREE_DISCONNECT", wire.Create: "CREATE",
	wire.Close: "CLOSE", wire.Flush: "FLUSH", wire.Read: "READ", wire.Write: "WRITE", wire.Lock: "LOCK",
	wire.IOCTL: "IOCTL", wire.Cancel: "CANCEL", wire.Echo: "ECHO", wire.QueryDirectory: "QUERY_DIRECTORY",
	wire.ChangeNotify: "CHANGE_NOTIFY", wire.QueryInfo: "QUERY_INFO", wire.SetInfo: "SET_INFO", wire.OplockBreak: "OPLOCK_BREAK",
}

// traceNoStreams is the #535 variant: no FILE_NAMED_STREAMS, stream opens
// fail and LOCK returns STATUS_NOT_SUPPORTED.
var traceNoStreams = os.Getenv("S3SMB_TRACE_NOSTREAMS") != ""

// traceNames maps open file IDs to the name used in CREATE or the last rename.
var traceNames sync.Map

var traceZeros = make([]byte, 64<<10)

func allZero(data []byte) bool {
	for len(data) > 0 {
		n := min(len(data), len(traceZeros))
		if string(data[:n]) != string(traceZeros[:n]) {
			return false
		}
		data = data[n:]
	}
	return true
}

func traceName(id wire.FileID) string {
	if name, ok := traceNames.Load(id); ok {
		return name.(string)
	}
	return "?"
}

func (connection *connection) trace(ctx context.Context, message wire.Message, result reply, status smb.Status, started time.Time) {
	command := message.Header.Command
	name, ok := traceCommands[command]
	if !ok {
		name = "UNKNOWN"
	}
	attrs := []any{"cmd", name, "mid", message.Header.MessageID, "status", uint32(status), "us", time.Since(started).Microseconds()}
	id := result.fileID
	path := func(requestID wire.FileID) {
		if id == (wire.FileID{}) {
			id = requestID
		}
		attrs = append(attrs, "fid", id.Volatile, "path", traceName(id))
	}
	switch command {
	case wire.Create:
		if create, err := wire.DecodeCreateRequest(message); err == nil {
			base, stream, _ := strings.Cut(create.Name, ":")
			attrs = append(attrs, "fid", id.Volatile, "path", base, "stream", stream, "disp", create.Disposition,
				"opts", create.Options, "access", create.DesiredAccess, "share", create.ShareAccess)
			// CreateAction is at body offset 4 and EndOfFile at 48.
			if status == smb.StatusSuccess && len(result.body) >= 56 {
				attrs = append(attrs, "action", binary.LittleEndian.Uint32(result.body[4:]), "size", binary.LittleEndian.Uint64(result.body[48:]))
			}
			if status == smb.StatusSuccess && id != (wire.FileID{}) {
				traceNames.Store(id, create.Name)
			}
			attrs = append(attrs, traceCreateContexts(connection, message, create, result, status)...)
		}
	case wire.Close:
		if request, err := wire.DecodeCloseRequest(message); err == nil {
			path(request.ID)
			if status == smb.StatusSuccess {
				traceNames.Delete(id)
			}
		}
	case wire.Flush:
		if flush, err := wire.DecodeFlushRequest(message); err == nil {
			path(flush.ID)
			variant := "data"
			if flush.Reserved1 == 0xffff {
				variant = "full"
			}
			attrs = append(attrs, "flush", variant)
		}
	case wire.Read:
		if read, err := wire.DecodeReadRequest(message); err == nil {
			path(read.ID)
			attrs = append(attrs, "off", read.Offset, "len", read.Length)
		}
	case wire.Write:
		if write, err := wire.DecodeWriteRequest(message); err == nil {
			path(write.ID)
			attrs = append(attrs, "off", write.Offset, "len", len(write.Data), "zero", allZero(write.Data), "wflags", write.Flags)
			if write.Offset == 0 {
				attrs = append(attrs, "head", hex.EncodeToString(write.Data[:min(len(write.Data), 64)]))
				if len(write.Data) >= 528 {
					attrs = append(attrs, "at512", hex.EncodeToString(write.Data[512:528]))
				}
			}
		}
	case wire.Lock:
		if lock, err := wire.DecodeLockRequest(message); err == nil {
			path(lock.ID)
		}
	case wire.IOCTL:
		if ioctl, err := wire.DecodeIOCTLRequest(message); err == nil {
			path(ioctl.ID)
			attrs = append(attrs, "ctl", ioctl.ControlCode)
		}
	case wire.QueryDirectory:
		if query, err := wire.DecodeQueryDirectoryRequest(message); err == nil {
			path(query.ID)
			attrs = append(attrs, "class", uint8(query.InfoClass), "pattern", query.Pattern, "qflags", query.Flags)
		}
	case wire.QueryInfo:
		if query, err := wire.DecodeQueryInfoRequest(message); err == nil {
			path(query.ID)
			attrs = append(attrs, "type", uint8(query.InfoType), "class", query.InfoClass)
			if query.InfoType == wire.InfoFilesystem && query.InfoClass == 5 {
				attrs = append(attrs, "fsattr", traceFSAttributes())
			}
		}
	case wire.SetInfo:
		if info, err := wire.DecodeSetInfoRequest(message); err == nil {
			path(info.ID)
			attrs = append(attrs, "type", uint8(info.InfoType), "class", info.InfoClass)
			if info.InfoType == wire.InfoFile {
				attrs = append(attrs, traceSetInfo(info, id, status)...)
			}
		}
	}
	opens, files := connection.server.options.State.TraceCounts()
	attrs = append(attrs, "opens", opens, "files", files)
	connection.server.options.Logger.Log(ctx, slog.LevelInfo, "smb trace", attrs...)
}

func traceSetInfo(info wire.SetInfoRequest, id wire.FileID, status smb.Status) []any {
	switch info.InfoClass {
	case uint8(wire.ClassFileEndOfFile):
		if eof, err := wire.DecodeFileEndOfFileInformation(info.Input); err == nil {
			return []any{"eof", eof.EndOfFile}
		}
	case uint8(wire.ClassFileAllocation):
		if allocation, err := wire.DecodeFileAllocationInformation(info.Input); err == nil {
			return []any{"alloc", allocation.AllocationSize}
		}
	case uint8(wire.ClassFileRename):
		if rename, err := wire.DecodeFileRenameInformation(info.Input); err == nil {
			if status == smb.StatusSuccess {
				traceNames.Store(id, strings.TrimPrefix(rename.Name, "\\"))
			}
			return []any{"target", rename.Name, "replace", rename.ReplaceIfExists}
		}
	case uint8(wire.ClassFileDisposition):
		if disposition, err := wire.DecodeFileDispositionInformation(info.Input); err == nil {
			return []any{"delete", disposition.DeletePending}
		}
	}
	return nil
}

// traceCreateContexts logs the CREATE contexts asked for and the lease and
// durable handle granted (#535).
func traceCreateContexts(connection *connection, message wire.Message, create wire.CreateRequest, result reply, status smb.Status) []any {
	names := make([]string, 0, len(create.Contexts))
	for _, c := range create.Contexts {
		names = append(names, c.Name)
	}
	attrs := []any{"ctx", strings.Join(names, ","), "oplock", create.OplockLevel}
	if contexts, err := decodeCreateContexts(create); err == nil {
		if contexts.lease != nil {
			attrs = append(attrs, "lease_req", contexts.lease.State, "lease_ver", contexts.lease.Version)
		}
		if contexts.durable != nil {
			attrs = append(attrs, "dh2q_timeout", contexts.durable.Timeout, "dh2q_flags", contexts.durable.Flags)
		}
		if contexts.reconnect != nil || contexts.legacyReconnect {
			attrs = append(attrs, "reconnect", true)
		}
	}
	if status == smb.StatusSuccess && result.fileID != (wire.FileID{}) {
		binding := state.Binding{SessionID: message.Header.SessionID, TreeID: message.Header.TreeID}
		if open, lease, found := connection.server.options.State.LeaseForOpen(state.FileID(result.fileID), binding); found == smb.StatusSuccess {
			attrs = append(attrs, "lease_granted", lease.State, "durable", open.Durable)
		}
	}
	return attrs
}

func traceFSAttributes() uint32 {
	if traceNoStreams {
		return smb.AdvertisedFilesystemAttributes &^ smb.FileNamedStreams
	}
	return smb.AdvertisedFilesystemAttributes
}
