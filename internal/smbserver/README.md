# Native SMB composition

`New(share, account, password string, filesystem vfs.VFSFileSystem) (*Server, error)` uses the bundled native VFS directly. The account must be named; an explicitly empty password still uses normal NTLM/signing, never guest/null mode. Configuration must reject an omitted password and warn about explicitly exposed passwordless binding. `New` does not listen or change bind policy.

The lifecycle owner opens a `net.Listener` and calls `Serve(listener)`. `Shutdown(ctx)` stops accepts/connections, waits for outstanding native I/O, flushes/closes connection handles and waits for protocol workers. Only after draining may the owner shut down the VFS, native storage and state lock. A deadline or cleanup error must be reported; a deadline does not grant permission to close resources still in use. Server instances are single-use, including shutdown-before-serve.

The adapter implements `internal/smb2/vfs.VFSFileSystem`, native `ByteRangeLocker`, and optionally `LockContext(context.Context, vfs.VfsHandle, []vfs.ByteRangeLock) error`. The latter receives connection cancellation. CLOSE cancels/relinquishes handle locks through the native adapter. The protocol owns write-through: successful normal and xattr writes call `Flush` for the request flag or `FILE_WRITE_THROUGH` create option. Adapter `Flush` must perform native durable synchronization, including metadata, and be valid for directory/read-only handles.

## Evidence and limits

Run all bundled upstream tests plus local regressions:

```
GOMAXPROCS=2 go test -p 2 -race ./internal/smb2/... ./internal/smbserver -count=1 -timeout 60s
```

Real TCP tests cover password/named-empty authentication, rejected wrong/unknown credentials, non-guest session flags, required signing, invalid signatures, reserved message IDs, concurrent authentication, unauthenticated tree rejection, abrupt disconnect cleanup and injected backend errors decoded by an independent SMB client. Related CREATE/WRITE/FLUSH/CLOSE compounds are tested over TCP with a known-key authenticated SMB 2.1 session fixture; NTLM negotiation is tested separately. Handler tests cover errno statuses, write-through and malformed bounds. Resource-fork logging tests use an unregistered secret marker with active debug JSON logging.

The fault-injecting VFS tests are **not** SMB-to-MinIO, crash/power-loss, complete dialect interoperability or Mac Time Machine evidence. A separate `TestMinIOSMBNativeByteRangeLocks` composes signed TCP locks, the real native adapter/SQLite and MinIO; it uses a known-key session, not an NTLM handshake. The executable's authentication and cold/crash recovery are separately tested by `test/e2e`. The native AAPL/full-sync capability advertisement is preserved and tested; advertisement is not proof of a completed backup. Durable reconnect/lease interoperability and full Mac acceptance remain external gates. Resource-fork offset writes and resize now preserve existing bytes, including concurrent sessions, with explicit native errors rather than silent replacement. Named xattr streams remain bounded by JuiceFS's native 65,536-byte xattr limit; larger requests fail. These narrow corrections are covered by handler and TCP regressions, not a claim of general large-stream or Mac compatibility.

See `docs/vendored.md` for the upstream source and the changes made to it.
