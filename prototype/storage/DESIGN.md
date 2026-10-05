# Storage interface v2: design

Goal: dead simple code that runs for years. Interface: [storage.go](storage.go). Layout: [LAYOUT.md](LAYOUT.md).

- **Methods.** `Storage`: `Lookup`, `Create`, `Sync`, `Close`. `File`: `Stat`, `List`, `ReadAt`, `WriteAt`, `Truncate`, `Flush`, `Rename`, `Delete`.
- **Two rules.** Data changes (`WriteAt`, `Truncate`) stay in memory until `Flush` or `Sync`. Namespace changes (`Create`, `Rename`, `Delete`) are in S3 when they return.

## The owner's questions

1. **Is 10 minimal?** In operations, yes: each method serves a request Time Machine sends, or startup and shutdown. v2 has 12, because `Lookup` is now a method and `Sync` is split from `Flush` (a bool that switches between one file and all files). The real cost is the work the server does around the calls, and v2 cuts that.
2. **Handles?** Yes to handles, no to Open and Close. The engine already keeps a node per file in memory (#522), so handing it out is free. smbfs renamed an open `mapped/180` to `.smbdelete…` while another open held a lease, then deleted it. With paths, the server must rewrite every open at or below a renamed path and keep I/O off it during the copy. With a `File`, opens just keep it. A path can also name a new file after a delete and create, as the plists do every backup. A `File` cannot. Opens, leases and durable handles stay in the server, so a storage Close would only be a second open table that can leak.
3. **Copy and Delete?** No. Time Machine never copies. The server would have to learn the crash order (chunk 0 copied last, deleted first, `Info.plist` last) and hide the half-copied target. A copy is a new file, so opens on the source would end up on a deleted file.
4. **Indirection map?** Renames are rare and small: about 7 plist renames per backup (one COPY and one DELETE, under 1 KiB each), and one directory rename per backup disk (about 2 bands, about 64 server-side COPYs). A first backup PUTs about 1 GB. A map is stored metadata (#522). A crash between the map and the chunks leaves orphans that need a sweep (#507). `cat` recovery would need a map parser (#529). Its one benefit, identity across renames, is what the in-memory `File` gives.
5. **SetSize?** Real SMB: SET_INFO EndOfFile (103 per backup) and CREATE with overwrite. Most of the 103 confirm a size a write already reached, and are now a no-op. `Info.plist` and `Info.bckup` do EOF 0, EOF new size, write. A shrink cannot be a write, so the method stays, named `Truncate` as in `os.File`.
6. **Shrink or deepen?** Deepen. `File` hides chunks, S3, crash order and identity. Two rules replace per-method timing. One code path writes file data (`Flush`, also used by `Sync` and early uploads). `ErrInvalid` defines what v1 left open: wrong-kind calls and paths too long for a key.

## Rejected alternatives

| Picked | Rejected | Why |
|---|---|---|
| `File` handles | a path in every call | Handles follow renames. Paths push open bookkeeping and locking into the server. |
| `Rename` | `Copy` + `Delete` | One crash order, tested in one place. Open handles survive. |
| path keys | indirection map | Renames cost about 14 tiny requests per backup. A map is metadata, makes orphans and ends `cat` recovery. |
| `Truncate` at `Flush` | `Truncate` straight to S3 | Below. |

## One changed decision

[#524](https://github.com/djosh34/s3-smb/issues/524) (round 5 (2)) does every S3 step of a shrink before the reply. v2 does them at the next `Flush`, in the same order. EOF 0 on `Info.plist`, the file that marks a valid bundle, would otherwise PUT an empty one, and a crash before the FLUSH leaves it empty. In v2 it goes from old to new bytes in one PUT. SMB promises durability only on FLUSH, so losing an unflushed shrink is allowed, like losing a write. It also leaves one code path that writes chunks. No other decision changes.
