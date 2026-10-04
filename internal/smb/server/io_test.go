package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// holdFlush makes storage Flush wait. Each Flush sends its mode on the first
// channel, then waits for its result on the second: nil runs the real flush.
func holdFlush(srv *testServer) (<-chan smb.SyncMode, chan<- error) {
	modes, results := make(chan smb.SyncMode), make(chan error)
	srv.faults.set(func(hooks *storageHooks) {
		hooks.Flush = func(ctx context.Context, handle smb.Handle, mode smb.SyncMode) error {
			select {
			case modes <- mode:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case err := <-results:
				if err != nil {
					return err
				}
				return srv.adapter.Flush(ctx, handle, mode)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	return modes, results
}

// flushResult is what a held Flush returns: an I/O error if fails, else the
// real flush.
func flushResult(fails bool) error {
	if fails {
		return smb.ErrIO
	}
	return nil
}

func TestRead(t *testing.T) {
	client := newTestServer(t).connect(t)
	writer, reader := client.open(t, "file"), client.open(t, "file")
	if status := client.write(t, wire.WriteRequest{ID: writer, Offset: 2, Data: []byte("hello")}); status != smb.StatusSuccess {
		t.Fatalf("WRITE status %#x", status)
	}
	owner, locked := client.open(t, "locked"), client.open(t, "locked")
	writeFile(t, client, owner, []byte("data"))
	lockRange(t, client, owner, 0, 64, lockExclusive, smb.StatusSuccess)
	directory := openDirectory(t, client, "directory")
	closed := client.open(t, "closed")
	closeOK(t, client, closed)
	for _, test := range []struct {
		name, data      string
		id              wire.FileID
		offset          uint64
		length, minimum uint32
		want            smb.Status
	}{
		{"short read", "hello", reader, 2, 10, 0, smb.StatusSuccess},
		{"minimum met", "hello", reader, 2, 10, 5, smb.StatusSuccess},
		{"minimum unmet", "", reader, 2, 10, 6, smb.StatusEndOfFile},
		{"minimum above length", "", reader, 2, 3, 4, smb.StatusEndOfFile},
		{"at end of file", "", reader, 7, 1, 0, smb.StatusEndOfFile},
		{"past end of file", "", reader, 8, 1, 0, smb.StatusEndOfFile},
		{"zero length", "", reader, 7, 0, 0, smb.StatusSuccess},
		{"zero length with minimum", "", reader, 7, 0, 1, smb.StatusEndOfFile},
		{"hole", "\x00\x00", reader, 0, 2, 0, smb.StatusSuccess},
		{"last signed offset", "", reader, math.MaxInt64 - 1, 1, 0, smb.StatusEndOfFile},
		{"empty at the signed limit", "", reader, math.MaxInt64, 0, 0, smb.StatusSuccess},
		{"past the signed limit", "", reader, math.MaxInt64, 1, 0, smb.StatusInvalidParameter},
		{"ending past the signed limit", "", reader, math.MaxInt64 - 1, 2, 0, smb.StatusInvalidParameter},
		{"offset past the signed limit", "", reader, math.MaxInt64 + 1, 0, 0, smb.StatusInvalidParameter},
		{"locked by another open", "", locked, 1, 1, 0, smb.StatusFileLockConflict},
		{"empty inside a lock", "", locked, 1, 0, 0, smb.StatusSuccess},
		{"empty past end of file inside a lock", "", locked, 64, 0, 0, smb.StatusSuccess},
		{"directory", "", directory, 0, 10, 0, smb.StatusInvalidDeviceRequest},
		{"empty from a directory", "", directory, 0, 0, 0, smb.StatusInvalidDeviceRequest},
		{"closed open", "", closed, 0, 1, 0, smb.StatusFileClosed},
	} {
		data, status := client.read(t, wire.ReadRequest{ID: test.id, Offset: test.offset, Length: test.length, MinimumCount: test.minimum})
		if status != test.want || string(data) != test.data {
			t.Errorf("%s: READ = %q, %#x; want %q, %#x", test.name, data, status, test.data, test.want)
		}
	}
}

func TestReadWriteLimits(t *testing.T) {
	client := newTestServer(t).connect(t)
	id := client.open(t, "file")
	if status := client.write(t, wire.WriteRequest{ID: id, Data: make([]byte, smb.MaxWriteSize)}); status != smb.StatusSuccess {
		t.Fatalf("largest WRITE: status %#x", status)
	}
	if _, status := client.read(t, wire.ReadRequest{ID: id, Length: smb.MaxReadSize}); status != smb.StatusSuccess {
		t.Fatalf("largest READ: status %#x", status)
	}
	for _, test := range []struct {
		name    string
		body    []byte
		command wire.Command
		charge  uint16
	}{
		{"WRITE too large", encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Data: make([]byte, smb.MaxWriteSize+1)}), wire.Write, creditsFor(int(smb.MaxWriteSize) + 1)},
		{"READ too large", encode(t, wire.EncodeReadRequest, wire.ReadRequest{ID: id, Length: smb.MaxReadSize + 1}), wire.Read, creditsFor(int(smb.MaxReadSize) + 1)},
		{"WRITE beyond its credits", encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Data: make([]byte, smb.CreditUnit+1)}), wire.Write, 1},
		{"READ beyond its credits", encode(t, wire.EncodeReadRequest, wire.ReadRequest{ID: id, Length: smb.CreditUnit + 1}), wire.Read, 1},
		{"WRITE on an RDMA channel", encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Channel: 1}), wire.Write, 1},
		{"READ on an RDMA channel", encode(t, wire.EncodeReadRequest, wire.ReadRequest{ID: id, Channel: 1}), wire.Read, 1},
		{"WRITE past the last byte", encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Offset: math.MaxUint64, Data: []byte("x")}), wire.Write, 1},
	} {
		if status := client.call(t, test.command, test.body, test.charge).Header.Status; status != smb.StatusInvalidParameter {
			t.Errorf("%s: status %#x", test.name, status)
		}
	}
	if status := client.write(t, wire.WriteRequest{ID: id}); status != smb.StatusSuccess {
		t.Fatalf("empty WRITE: status %#x", status)
	}
}

// READ needs READ_DATA or EXECUTE; WRITE needs WRITE_DATA, or APPEND_DATA
// for writes at the end of the file.
func TestReadWriteNeedGrantedAccess(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "file", "data")
	for _, test := range []struct {
		access          uint32
		read, overwrite smb.Status
		append          smb.Status
	}{
		{0x80, smb.StatusAccessDenied, smb.StatusAccessDenied, smb.StatusAccessDenied},
		{fileReadData, smb.StatusSuccess, smb.StatusAccessDenied, smb.StatusAccessDenied},
		{fileExecute, smb.StatusSuccess, smb.StatusAccessDenied, smb.StatusAccessDenied},
		{fileWriteData, smb.StatusAccessDenied, smb.StatusSuccess, smb.StatusSuccess},
		{fileAppendData, smb.StatusAccessDenied, smb.StatusAccessDenied, smb.StatusSuccess},
	} {
		id := openAs(t, client, "file", test.access, 7, 0)
		if _, status := client.read(t, wire.ReadRequest{ID: id, Length: 4}); status != test.read {
			t.Errorf("access %#x: READ status %#x, want %#x", test.access, status, test.read)
		}
		if status := client.write(t, wire.WriteRequest{ID: id, Data: []byte("data")}); status != test.overwrite {
			t.Errorf("access %#x: WRITE at 0 status %#x, want %#x", test.access, status, test.overwrite)
		}
		end := uint64(len(srv.content(t, "file")))
		if status := client.write(t, wire.WriteRequest{ID: id, Offset: end, Data: []byte("+")}); status != test.append {
			t.Errorf("access %#x: WRITE at the end status %#x, want %#x", test.access, status, test.append)
		}
		closeOK(t, client, id)
	}
}

func TestReadWriteStorageErrors(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	id := client.open(t, "file")
	for name, test := range map[string]struct {
		hooks storageHooks
		write bool
		want  smb.Status
	}{
		"read error after bytes":         {storageHooks{ReadAt: readResult(2, smb.ErrIO)}, false, smb.StatusIODeviceError},
		"end of file after bytes":        {storageHooks{ReadAt: readResult(2, io.EOF)}, false, smb.StatusSuccess},
		"storage error with end of file": {storageHooks{ReadAt: readResult(2, errors.Join(smb.ErrIO, io.EOF))}, false, smb.StatusIODeviceError},
		"more bytes than asked for":      {storageHooks{ReadAt: readResult(5, nil)}, false, smb.StatusIODeviceError},
		"disk full":                      {storageHooks{WriteAt: writeResult(0, smb.ErrDiskFull)}, true, smb.StatusDiskFull},
		"short write":                    {storageHooks{WriteAt: writeResult(2, nil)}, true, smb.StatusIODeviceError},
		"write-through flush error":      {storageHooks{Flush: func(context.Context, smb.Handle, smb.SyncMode) error { return smb.ErrIO }}, true, smb.StatusIODeviceError},
	} {
		srv.faults.set(func(hooks *storageHooks) { *hooks = test.hooks })
		var status smb.Status
		if test.write {
			status = client.write(t, wire.WriteRequest{ID: id, Data: []byte("data"), Flags: writeThrough})
		} else {
			_, status = client.read(t, wire.ReadRequest{ID: id, Length: 4})
		}
		if status != test.want {
			t.Errorf("%s: status %#x, want %#x", name, status, test.want)
		}
	}
}

// readResult makes ReadAt fill the buffer with "data" and return n and err.
func readResult(n int, err error) func(context.Context, smb.Handle, []byte, uint64) (int, error) {
	return func(_ context.Context, _ smb.Handle, dst []byte, _ uint64) (int, error) {
		copy(dst, "data")
		return n, err
	}
}

func writeResult(n int, err error) func(context.Context, smb.Handle, []byte, uint64) (int, error) {
	return func(context.Context, smb.Handle, []byte, uint64) (int, error) { return n, err }
}

// A write-through WRITE, asked for by the WRITE or by its CREATE, replies only
// after storage has flushed the data.
func TestWriteThroughWaitsForFlush(t *testing.T) {
	for _, test := range []struct {
		name           string
		flags, options uint32
		want           smb.Status
		fails          bool
	}{
		{"WRITE flag", writeThrough, 0, smb.StatusSuccess, false},
		{"CREATE option", 0, fileWriteThrough, smb.StatusSuccess, false},
		{"failed flush", writeThrough, 0, smb.StatusIODeviceError, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t)
			client := srv.connect(t)
			id := openAs(t, client, "file", fileAllAccess, 7, test.options)
			modes, results := holdFlush(srv)
			request := client.send(t, wire.Write, encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: id, Data: []byte("durable"), Flags: test.flags}), 1)
			if mode := <-modes; mode != smb.SyncData {
				t.Fatalf("flush mode %d", mode)
			}
			client.interim(t, request)
			client.echo(t)
			results <- flushResult(test.fails)
			if status := client.receive(t, request).Header.Status; status != test.want {
				t.Fatalf("WRITE status %#x, want %#x", status, test.want)
			}
		})
	}
}

// FLUSH of any open covers the file's data. FLUSH with Reserved1 0xffff is
// the full sync macOS sends; both reply only after storage has flushed.
func TestFlushWaitsForStorage(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	writer, other := client.open(t, "file"), client.open(t, "file")
	writeFile(t, client, writer, []byte("data"))
	modes, results := holdFlush(srv)
	for _, test := range []struct {
		want     smb.Status
		reserved uint16
		mode     smb.SyncMode
		fails    bool
	}{
		{smb.StatusSuccess, 0, smb.SyncData, false},
		{smb.StatusSuccess, 0xffff, smb.SyncFull, false},
		{smb.StatusIODeviceError, 0xffff, smb.SyncFull, true},
	} {
		request := client.send(t, wire.Flush, encode(t, wire.EncodeFlushRequest, wire.FlushRequest{ID: other, Reserved1: test.reserved}), 1)
		if mode := <-modes; mode != test.mode {
			t.Fatalf("Reserved1 %#x: flush mode %d", test.reserved, mode)
		}
		client.interim(t, request)
		client.echo(t)
		results <- flushResult(test.fails)
		if status := client.receive(t, request).Header.Status; status != test.want {
			t.Fatalf("Reserved1 %#x: status %#x, want %#x", test.reserved, status, test.want)
		}
	}

	srv.faults.set(func(hooks *storageHooks) {
		hooks.Flush = func(context.Context, smb.Handle, smb.SyncMode) error {
			t.Error("invalid FLUSH reached storage")
			return smb.ErrIO
		}
	})
	closed := other
	closed.Volatile++
	for _, test := range []struct {
		body []byte
		want smb.Status
	}{
		{encode(t, wire.EncodeFlushRequest, wire.FlushRequest{ID: other, Reserved1: 1}), smb.StatusInvalidParameter},
		{encode(t, wire.EncodeFlushRequest, wire.FlushRequest{ID: other, Reserved1: 0x8000}), smb.StatusInvalidParameter},
		{encode(t, wire.EncodeFlushRequest, wire.FlushRequest{ID: other, Reserved1: 0xfffe}), smb.StatusInvalidParameter},
		{encode(t, wire.EncodeFlushRequest, wire.FlushRequest{ID: other})[:10], smb.StatusInvalidParameter},
		{encode(t, wire.EncodeFlushRequest, wire.FlushRequest{ID: closed}), smb.StatusFileClosed},
	} {
		if status := client.call(t, wire.Flush, test.body, 1).Header.Status; status != test.want {
			t.Errorf("FLUSH %x: status %#x, want %#x", test.body, status, test.want)
		}
	}
}

// An append-only WRITE checks the end of file when it reaches storage, so a
// WRITE that extended the file meanwhile makes it fail instead of being
// overwritten.
func TestAppendWriteRechecksEndOfFile(t *testing.T) {
	for _, test := range []struct {
		name, path string
		winner     uint32
	}{
		{"file, writer wins", "file", fileReadData | fileWriteData},
		{"file, appender wins", "file", fileReadData | fileAppendData},
		{"stream, writer wins", "file:fork", fileReadData | fileWriteData},
		{"stream, appender wins", "file:fork", fileReadData | fileAppendData},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := newTestServer(t)
			client := srv.connect(t)
			seed(t, client, "file", "base")
			seed(t, client, "file:other", "other stream")
			seed(t, client, test.path, "seed")
			appender := openAs(t, client, test.path, fileReadData|fileAppendData, 7, 0)
			winner := openAs(t, client, test.path, test.winner, 7, 0)
			entered, release := make(chan struct{}), make(chan struct{})
			srv.faults.set(func(hooks *storageHooks) {
				hooks.WriteAt = func(ctx context.Context, handle smb.Handle, src []byte, offset uint64) (int, error) {
					if string(src) == "stale" {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return 0, ctx.Err()
						}
					}
					return srv.adapter.WriteAt(ctx, handle, src, offset)
				}
			})
			request := client.send(t, wire.Write, encode(t, wire.EncodeWriteRequest, wire.WriteRequest{ID: appender, Offset: 4, Data: []byte("stale")}), 1)
			<-entered
			client.interim(t, request)
			if status := client.write(t, wire.WriteRequest{ID: winner, Offset: 4, Data: []byte("winner")}); status != smb.StatusSuccess {
				t.Fatalf("winning WRITE: status %#x", status)
			}
			close(release)
			if status := client.receive(t, request).Header.Status; status != smb.StatusAccessDenied {
				t.Fatalf("stale append: status %#x", status)
			}
			want := map[string]string{"file": "base", "file:other": "other stream"}
			want[test.path] = "seedwinner"
			srv.expectContent(t, want)
		})
	}
}

// Supersede with only APPEND_DATA empties the file but still allows only
// writes at its end.
func TestAppendSupersede(t *testing.T) {
	srv := newTestServer(t)
	client := srv.connect(t)
	seed(t, client, "file", "old data")
	request := wire.CreateRequest{Name: "file", DesiredAccess: fileDelete | fileReadData | fileAppendData, ShareAccess: 7, Disposition: fileSupersede}
	reply := mustOpen(t, client, request)
	if reply.Size != 0 || reply.Action != 0 {
		t.Fatalf("supersede reply %+v", reply)
	}
	for _, test := range []struct {
		data   string
		offset uint64
		want   smb.Status
	}{{"new", 0, smb.StatusSuccess}, {"bad", 0, smb.StatusAccessDenied}, {" tail", 3, smb.StatusSuccess}} {
		if status := client.write(t, wire.WriteRequest{ID: reply.ID, Offset: test.offset, Data: []byte(test.data)}); status != test.want {
			t.Fatalf("WRITE %q at %d: status %#x, want %#x", test.data, test.offset, status, test.want)
		}
	}
	srv.expectContent(t, map[string]string{"file": "new tail"})
}

// An allocation that waits in storage cannot grow the file back after
// another open shrank it meanwhile.
func TestAllocationCannotUndoAConcurrentShrink(t *testing.T) {
	for _, path := range []string{"file", "file:fork"} {
		t.Run(path, func(t *testing.T) {
			srv := newTestServer(t)
			client, other := srv.connect(t), srv.connect(t)
			seed(t, client, "file", "base")
			allocator := client.open(t, path)
			writeFile(t, client, allocator, bytes.Repeat([]byte("a"), 9000))
			shrinker := other.open(t, path)
			entered, release := make(chan struct{}), make(chan struct{})
			srv.faults.set(func(hooks *storageHooks) {
				hooks.SetAttr = func(ctx context.Context, object smb.ObjectKey, change smb.AttrChange) error {
					srv.faults.set(func(hooks *storageHooks) { hooks.SetAttr = nil })
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					return srv.adapter.SetAttr(ctx, object, change)
				}
			})
			input := encode(t, wire.EncodeFileAllocationInformation, wire.FileAllocationInformation{AllocationSize: 4097})
			body := encode(t, wire.EncodeSetInfoRequest, wire.SetInfoRequest{ID: allocator, InfoType: wire.InfoFile, InfoClass: uint8(wire.ClassFileAllocation), Input: input})
			request := client.send(t, wire.SetInfo, body, 1)
			<-entered
			if status := setEndOfFile(t, other, shrinker, 7); status != smb.StatusSuccess {
				t.Fatalf("shrink: status %#x", status)
			}
			close(release)
			if status := client.receive(t, request).Header.Status; status != smb.StatusSuccess {
				t.Fatalf("allocation: status %#x", status)
			}
			want := map[string]string{"file": "base"}
			want[path] = "aaaaaaa"
			srv.expectContent(t, want)
		})
	}
}
