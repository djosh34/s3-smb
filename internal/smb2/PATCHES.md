# Intentional SMB corrections

Base: macos-fuse-t/go-smb2 commit `277a9300411249a881a05f7a910f5a83ae3395f2`. Upstream LICENSE and Attributions are retained. Packaging mechanically relocates imports; the upstream implementation remains the protocol engine.

Application patches for issue #23:

- `server/file_tree.go`, `status.go`: propagate FLUSH, CLOSE (including flush/unlink/close), WRITE and xattr setter failures; map wrapped native errno values to meaningful statuses; perform native Flush for request/create write-through; remove raw xattr-value diagnostics. Validate handle ownership by session, tree and persistent/volatile IDs. Preserve zero-length writes and native Apple negotiation.
- `server/server.go`, `cleanup.go`, `conn.go`: connection-local SPNEGO/NTLM and negotiated dialect state; initialize the native durable-open map; reject unauthenticated work; publish completed session keys before the client can send its next request. Close handles on disconnect/logoff/tree disconnect, drain I/O and protocol workers, expose deadline-aware shutdown and propagate cleanup failures. Stop on listener errors rather than spinning. Make queue shutdown cancellation-aware.
- `server/conn.go`, `compound.go`: failed signature verification terminates the connection before dispatch. Reserved client message IDs are rejected, not exempted from verification. Resolve inherited related-compound session/tree IDs for authorization without rewriting signed request bytes. Validate the complete compound chain before dispatch so its final response ID is known; sign compound response padding as transmitted.
- `server/request_validation.go`, `internal/smb2/request.go`: validate file-request framing/ownership and CREATE context/WRITE bounds before slicing; use the actual WRITE data offset and widened arithmetic. Reject missing-leading/nonrelated inherited-session sentinels and malformed compound offsets.
- `server/xattr.go`, `file_tree.go`, `vfs/xattr.go`: preserve resource-fork bytes across offset writes and resizing, with the native 65,536-byte bound checked before allocation. Serialize stream read/modify/write, resize, create-time truncation, whole setters and removal across sessions. Do not turn a native xattr-read error into destructive creation/truncation. Propagate ordinary file truncation errors instead of reporting success.
- `server/log.go`: logging-owner bridge to the application's slog pipeline; not an authentication or filesystem replacement.

New tests live beside the upstream tests: `auth_wire_test.go`, `session_gate_test.go`, `compound_wire_test.go`, `durability_test.go`, `error_wire_test.go`, `cleanup_test.go`, `lock_wire_test.go`, `malformed_test.go`, `capabilities_test.go`, `redaction_test.go`, and `xattr_*_test.go`. `lock_minio_integration_test.go` composes signed TCP locks with the real native filesystem and MinIO.

Evidence limits and the lifecycle/VFS integration contract are documented in `../smbserver/README.md`. No new guest mode, secondary filesystem/cache, bespoke lock manager or Time Machine implementation was introduced. Native durable reconnect/lease support is not newly asserted by these patches.
