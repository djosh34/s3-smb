# Research: panics, fatal exits and recover in s3-smb

Checked against main @ ac712a6. Paths are relative to `internal/` unless they start with `main.go` or `docs/`. Classes: **RT** = can happen while serving, **SU** = startup only (this includes recovery, which runs inside `serve`, app/serve.go:229), **UR** = cannot happen (no caller, constant input or an invariant that holds).

## 1. Explicit panic, Fatal and Exit calls

**Our packages.** `main.go:12` calls `os.Exit` with the normal return code. `app/main.go:97-100` `exitFailure` calls `os.Exit(1)`. It is reached from the 30 s hard-shutdown timer (app/serve.go:46, app/main.go:56) and when shutdown fails (serve.go:104). Both are deliberate exits at the end of the process. There are no `panic(` calls. There are implicit ones: storage/runtime.go:84 and :90 use unchecked type assertions on the `OnMsg` args (`args[0].(uint64)` and so on). They are safe because JuiceFS always passes those types, but they are still panics.

**JuiceFS** (44 sites; `logger.Fatal*` uses logrus and still calls `os.Exit(1)`, logging/native.go:15):

| Site | What | Class |
|---|---|---|
| meta/base.go:919,924,927 | `os.Exit(UmountCode)` in the heartbeat `refresh` goroutine (started by NewSession, serve.go:319) when the format row disappears, MetaVersion > Max, or the UUID changes | **RT**, but only if the SQLite DB is tampered with outside the process |
| meta/base.go:2868 | compaction overlap invariant. Runs in a `startMutableTask` goroutine (base.go:2138,3012), or synchronously inside `Write` from `commitThread` (base.go:2193) | **RT** (an invariant, in a background goroutine) |
| meta/dump.go:239,245,256,274,282,300,306,314; meta/sql.go:4796 | `panic(err)` on bufio write errors (for example a full staging disk) during the hourly `DumpMeta` (vfs/backup.go:112) | **RT**, contained by the JuiceFS recover at sql.go:4908 |
| meta/sql.go:4702 | `dumpDir` (non-fast) | UR (SQLite forces `fast`, sql.go:4904-4906) |
| meta/sql.go:5265,5272 | `logger.Fatalf` in LoadMeta insert goroutines on any SQLite insert error | **SU** (recovery) |
| meta/interface.go:310 `typeFromString` | panics on an unknown `"type"` string in a dump. Called from LoadMeta (sql.go:5141,5172,5183; dump.go:425,534,593). `inspectReader` checks only the root (backup/recovery.go:80) | **SU** (recovery from a corrupt dump) |
| meta/interface.go:268 `typeToStatType` | only through `Attr.SMode()` in `Entry.String()` (vfs/helpers.go:89, logging) | UR (needs a corrupt Typ in the DB) |
| chunk/cached_store.go:423 | block length mismatch inside the upload goroutine | RT in theory (invariant), UR in practice |
| chunk/cached_store.go:483 | `FlushTo` with offset < uploaded | UR (slen only grows) |
| vfs/compact.go:99 | `panic(err)` on a `FlushTo` error | UR (`FlushTo` always returns nil, cached_store.go:481-495) |
| chunk/page.go:48 | `NewOffPage(<=0)`. Reachable from disk_cache.go:1532 if a CsExtend cache file is truncated (size goes negative) | **RT** (needs local cache corruption) |
| chunk/cached_store.go:831 | unknown compression | SU, UR (validated at storage/volume.go:53) |
| meta/config.go:253 | `crypto/rand` failure in `Format.Encrypt` | SU, UR |
| meta/interface.go:635-655 | `NewClient` Fatalf. Not used (we call `NewSQLite`) | UR |
| meta/info.go:95 | Redis version check. No callers | UR |
| meta/sql.go:3502 | `CopyFileRange` invariant. smbfs never calls it | UR |
| meta/openfile.go:167, meta/base.go:764, vfs/reader.go:66, utils/alloc.go:42 | invariants | UR |
| chunk/disk_cache.go:1443, utils/buffer.go:169 | `init()` with constant input | UR |
| chunk/cache_eviction.go:132 | `none` policy. The default is 2-random (cached_store.go:623) | UR |
| utils/humanize.go:55,82; utils/utils.go:184; object/encrypt.go:58,164,294,307 | no callers (`NewChunkedEncrypted`, `ExportRsa…`, `NewKeyEncryptor` are unused; volume.go:175 uses `NewRSAEncryptor`) | UR |
| ~50 `MustRegister` calls (vfs.go:1392, base.go:547-611, cached_store.go:982…) | each Runtime uses a fresh `prometheus.NewRegistry()` (storage/runtime.go:93) | SU |

**thirdparty.** xorm dialects/dialect.go:183 and driver.go:21,24 are `init`-time registration (SU/UR). Its `MustCompile` calls take constant patterns (UR). mpb bar.go:106,486, progress.go:145 and decor/*:170,51,108,166,224 are all UR. JuiceFS uses them only through `utils.NewProgress` (output nil, progress.go:93), with constant formats and AverageSpeed, not EWMA.

**smb2.** vfs/attributes.go:82,113,130,145,204 are "mandatory attribute" getters. They are UR because smbfs/attributes.go:32-34 always sets all five. ccm.go:59,93 (fixed nonce 11) and cmac.go:65 (AES) are UR. **ccm.go:97 is RT for an authenticated client:** a 52-byte transform header with an empty body (packet.go:274, session.go:92-101) gives `len(ciphertext)==16`, which panics on a CCM session.

## 2. Implicit panic risks

- **JuiceFS hot paths:** low. About 42 unchecked type assertions in vfs/meta/chunk/fs. They are all pool `Get()` calls or xorm bean types (sql.go:4844-4888, sql_bak.go). Persisted blobs are bounds-checked (slice.go:118 `readSliceBuf`). `utils.Buffer.Get*` (buffer.go:108,135) has no checks, but the SQL backend stores attributes as columns. The upstream FUSE client has no top-level recover either, so these paths are well exercised.
- **SMB decoding:** the project already hardened it. `validateRequest` runs `IsInvalid` per command (server/request_validation.go:52-125), `validCreateContexts`, and there are 37 `IsInvalid` call sites. These remain, and each one crashes the whole process because the connection goroutines have no recover (server.go:338-339):
  - **Before authentication:** ntlm/server.go:195,206,217,228 check `int(off+uint32(len))` in uint32, so `off=0xFFFFFFFF,len=1` wraps the sum and passes the check, and the slice then panics. ntlm/server.go:240-245 `ntChallengeResponse[16:]`, `[8:16]`, `[16:24]`, `[28:]` have no length check once the user name matches (only the name is needed, not the password). Exposure is limited by the default `127.0.0.1` listen address.
  - **After authentication, with an open handle:** SetInfo `Buffer()` is `r[32:]` with no per-class minimum (request.go:1576). Affected: FileBasicInformation `f[32:36]` (info_fs.go:187, file_tree.go:2197), EndOfFile (2259), and Rename `f[20:20+FileNameLength]` (info_fs.go:1090, file_tree.go:2401). Also file_tree.go:986 logs `rd.ReparseTag()` (fscc.go:100 `c[:4]`) after `IsInvalid` failed because the buffer was shorter than 20 bytes. Plus the CCM case above.
  - About 7 concrete sites in total. There are no fuzz tests (no `func Fuzz`), and malformed_test.go covers only Write and compound.

## 3. Would a recover in our adapter leave JuiceFS deadlocked or inconsistent?

- **The synchronous path is mostly defer-safe.** smbfs `s.mu` (fs.go:340-341), `fs.File` lock (fs/fs.go:1338,1406,1454,1468,1484), vfs handle `Wlock`/`Rlock` (vfs.go:849,787), `fileWriter.Write`/`flush` (writer.go:343-344,388-389), meta `txBatchLock` (sql.go transaction), and xorm `Session.Close` all release on panic, and the session close rolls back an open transaction (thirdparty/xorm/session.go:153-160). Some state still leaks: `h.removeOp` is not deferred (vfs.go:853), and the `writewaiting`/`flushwaiting` counters can be left wrong (writer.go:346-353,390).
- **Most of the real work runs in JuiceFS-owned goroutines, so an adapter recover cannot catch it.** That includes reads (`go s.run()`, reader.go:243,331), flush, commit and ID prep (writer.go:144,174,197,283,289), uploads (cached_store.go:418), compaction and deletes (base.go:3012), the heartbeat os.Exit, the LoadMeta Fatalf goroutines and `utils.WithTimeout` (utils.go:115). A panic there kills the process whatever we do.
- **Some failures can't be recovered at all.** `sliceReader.run` (reader.go:164-230) and `commitThread` (writer.go:189-232) do `Lock(); defer Unlock()` with manual `Unlock()…Lock()` windows around `meta.Read` and `meta.Write`. A panic inside a window makes the deferred Unlock throw `sync: unlock of unlocked mutex`, which is a fatal runtime error that recover cannot catch. If `commitThread` dies, `f.chunks` never drains: `flush` hangs for at least 5 minutes and then returns EIO, and `dataWriter.free` never runs. `os.Exit` and logrus Fatal skip recover entirely.
- Lock style: defer and manual unlocks are mixed. Counts of defer/manual: vfs/writer.go 8/13, reader.go 6/9, handle.go 11/12, meta/base.go 16/32, chunk/disk_cache.go 7/19.

## 4. Existing recover() calls

- **chunk/cached_store.go:757-763:** upstream code. `load` turns any panic in block fetch or decompress (corrupt object) into an error. It does not cover the `WithTimeout` goroutine that calls `storage.Get`.
- **meta/sql.go:4908-4915:** an upstream recover, patched by us to not print the payload (docs/vendored.md:30). It exists only because dump.go uses `panic(err)` for write errors.
- **thirdparty/mpb/bar.go:331:** guards user decorators in the render goroutine.
- There is none in smb2, smbfs, app, backup or storage.

## 5. How it runs, and what a crash does

- It runs in the foreground in a terminal: "There is no daemon mode or service installer" (README.md:128). There is no launchd, `brew services` or systemd setup. **Nothing restarts it after a crash**, and "s3-smb must be running whenever Time Machine backs up" (README.md:73). It shuts down gracefully on SIGINT/SIGTERM (app/main.go:38). The first run and recovery both need an interactive `yes`.
- **Effect on a backup in progress:** the SMB connection drops and backupd fails the backup. Later scheduled backups fail until the user restarts s3-smb.
  - SMB WRITE is acknowledged after `Pwrite` puts the data in JuiceFS's memory buffer (`BufferSize` 300 MiB, storage/runtime.go:41). Data becomes durable only on FLUSH, close or O_SYNC (smbfs/fs.go:287,311,368 → `Fsync`), which uploads to S3 and commits to SQLite.
  - Writeback is not enabled, so there is no local staging and nothing to replay. Unflushed data is lost, as with a power cut on a local disk.
  - Blocks that were uploaded but not committed become orphan objects in S3.
  - Committed metadata survives because SQLite runs in WAL mode with `synchronous=FULL` (sql.go:454-459).
  - The macOS end-to-end test kills s3-smb during a second backup, restarts it and restores the first backup successfully (docs/development.md:104-108). Time Machine's sparsebundle tolerates this.

## 6. Recommendation

**Don't add a recover boundary around JuiceFS.** It would cover only the SMB goroutine's synchronous calls. The likely panic sites run in JuiceFS goroutines, call os.Exit, or are runtime throws. Where it did catch something, it would keep serving a backup target with in-memory writer, handle and openfile state that may be wrong, plus a possible 5-minute flush hang. For a backup product, failing loudly (process exits, nothing acknowledged after the last FLUSH is trusted, the next start reopens a consistent SQLite) is safer than continuing in a half-broken state. The product already relies on crash-safe behaviour and tests it.

**Smallest JuiceFS changes (about 10 sites):**
1. **LoadMeta, sql.go:5265,5272:** keep the first error from the insert goroutines and return it after `wg.Wait()`. This is the most valuable fix because it is the recovery path.
2. **typeFromString, interface.go:310:** return `(uint8, bool)`. `loadEntry` or `loadEntries` returns an error for an unknown type. Alternatively, validate every entry type in `inspectReader`.
3. **dump.go write helpers and sql.go:4796:** drop the `panic(err)`. `bufio.Writer` keeps the first error and `DumpMeta` already returns `bw.Flush()` (sql.go:5131). The `json.Marshal` calls on plain structs cannot fail. Then delete the recover at sql.go:4908.
4. **base.go:919-927:** replace `os.Exit` with a fatal callback (or cancelling a context) that the app wires into its normal shutdown with an error. You keep the exit status and the log, and gain an ordered shutdown.
5. **base.go:2868:** `logger.Errorf` and return, skipping that compaction. Compaction is optional.
6. **cached_store.go:423,483 and compact.go:99:** send the error through `s.errors` and have `FlushTo` return it. Return the error at compact.go:99.
7. **disk_cache.go:1532:** if `size<=0`, return an error, so a corrupt cache file is treated as a cache miss.
8. **Dead code:** delete or convert the UR sites in the table so a `grep panic(` audit comes back clean.

Keep the `cachedStore.load` recover, or validate decompressor input instead. In unencrypted mode it is the only protection against a corrupt S3 object crashing the server. With encryption on (the default), AES-GCM rejects a modified object before it reaches the decompressor.

**SMB fixes, which are more important than the JuiceFS ones:** compute bounds in `uint64`/`int` at ntlm/server.go:195-228 and check lengths before :240-245. Add per-class minimum buffer lengths to `setInfo`. Don't call `ReparseTag()` at file_tree.go:986 when the buffer is too short. In `decrypt`, reject a transform whose body is ≤ 16 bytes. Add fuzz tests for the NTLM authenticate and SetInfo decoders.

**What this costs:** errors replace crashes only on paths where a clean error is meaningful (recovery and dump). Background invariants turn into logged skips, which can hide bugs. Offset that with error-level logs and tests. You still have no supervisor. If the owner wants failures to recover on their own, a documented launchd `KeepAlive` plist does more for availability than any recover, but first-run initialisation and recovery need interactive confirmation.
