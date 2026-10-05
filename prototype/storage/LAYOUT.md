# Bucket layout (prototype v2)

Draft for [#516](https://github.com/djosh34/s3-smb/issues/516). Interface: [storage.go](storage.go). Choices: [DESIGN.md](DESIGN.md).

## Keys

| Key | What it is |
|---|---|
| `files/<path>/<index>` | Chunk `<index>` of the file at `<path>`. Raw bytes, 8 MiB, except the last chunk (0 to 8 MiB). |
| `files/<path>/` | Empty marker object: the directory at `<path>` exists. Every directory has one, so empty directories exist. The root has none. |
| `locks/<server id>` | Lock key of one server. Server ID is hostname plus bucket. Body unused, only Last-Modified counts. |

- **Chunk index:** 8 decimal digits, zero padded: `00000000`, `00000001`, ... Keys sort in index order, so `cat dir/*` concatenates in order. 8 digits allow files up to 800 TB.
- **Parsing:** the last segment of a chunk key is always the index, the rest is the path. `files/a/b/00000003` is chunk 3 of file `a/b`. A file inside directory `a/b` named `00000003` would be `files/a/b/00000003/00000000`, so nothing is ambiguous.
- **Data under `files/`** keeps it apart from `locks/`, so a share folder named `locks` is fine.
- **Path length:** a path whose longest key would pass S3's 1024 bytes is `ErrInvalid`.
- **Existence:** a file exists iff its chunk 0 exists. A directory exists iff its marker exists.
- **Size:** last index × 8 MiB + the last chunk's length. **Modified time:** newest chunk Last-Modified. Nothing else is stored.
- **Shape check:** a gap, or a chunk below the last that is not 8 MiB, is `ErrCorrupt`.

## When S3 is called

- `Lookup`, `Stat` and `List` never call S3. The namespace lives in memory.
- `WriteAt` and `Truncate` change memory only. `ReadAt` and `WriteAt` may GET a chunk that is not in memory.
- `Flush` and `Sync` write file data. Early uploads under memory pressure use the same code.
- `Create`, `Rename` and `Delete` write to S3 before they return.

## Time Machine operations and their S3 requests

`c/i` means `files/<path>/<i>`. Writes check the lock first.

| Operation | Method | S3 requests |
|---|---|---|
| Lookup, QUERY_INFO | `Lookup`, `Stat` | None. |
| List `bands/` | `List` | None. |
| Create file | `Create` | LIST `files/<path>/` and DELETE any leftovers, then PUT `c/0` (0 bytes). |
| Create directory | `Create` | PUT `files/<path>/`. |
| Write | `WriteAt` | None, data goes to memory. A GET `c/i` only to patch a chunk that is not in memory. |
| Read | `ReadAt` | None from memory. Otherwise a ranged GET per chunk touched. |
| SET_INFO EndOfFile, same size (most of the 103) | `Truncate` | None. |
| SET_INFO EndOfFile, other size (`Info.plist`: EOF 0, EOF new size, write) | `Truncate` | None until FLUSH. Then the FLUSH row below. `Info.plist` goes from old to new bytes in one PUT and is never empty in the bucket. |
| FLUSH | `Flush` | 1. DELETE `c/i` above the new last chunk, top down. 2. PUT dirty chunks below the old last chunk in parallel. 3. PUT the old last chunk and new chunks upward, one at a time. A crash never leaves a gap or a short chunk in the middle. |
| Full FLUSH | `Sync` | The same, for every file. Reply after all of them succeed. |
| Delete file | `Delete` | DELETE `c/0`, then the other chunks in parallel. |
| Delete directory | `Delete` | DELETE `files/<path>/` (must be empty). |
| File rename (`.plist.tmp` to `.plist`, or open `mapped/180` to `.smbdelete…`) | `Rename` | Flush the file. COPY `c/1..N`, COPY `c/0`. DELETE old `c/0`, DELETE old rest. Open handles keep the same `File`. |
| Directory rename (`.incomplete` to `.sparsebundle`) | `Rename` | Flush the files under it. PUT new markers, parents first. COPY every chunk except chunk 0s, then every `c/0`, `Info.plist` last. DELETE old `Info.plist` `c/0`, other old `c/0`s, old chunks, then old markers deepest first. |
| Startup | `Open` | LIST `locks/`, refuse if another key is younger than 10 min. PUT `locks/<id>`. LIST `locks/` again, on a fresh other key DELETE ours and refuse. DELETE keys older than 10 min. LIST `files/` (all pages). DELETE chunks with no `c/0`. Check every shape. |
| Running | | PUT `locks/<id>` every minute. No write after 8 min without a successful renewal. |
| Clean shutdown | `Close` | `Sync`, then DELETE `locks/<id>`. |

## Manual recovery without s3-smb

1. Download `files/` with any S3 tool, for example `rclone copy remote:bucket/files ./restore`. Skip `locks/`.
2. Every file is now a folder of chunks. Concatenate each one in place:

   ```sh
   cd restore
   find . -type f -name 00000000 | sort | while read -r c; do
     d=$(dirname "$c")
     cat "$d"/[0-9]* > "$d.part" && rm -r "$d" && mv "$d.part" "$d"
   done
   ```
3. Open the `.sparsebundle` on a Mac (double-click or `hdiutil attach`), or serve it over SMB and use "Browse other backup disks". An encrypted backup needs its password.

## Open questions

1. **Growing FLUSH order.** New tail chunks are PUT one after another to keep files gap free. Filling a band then costs up to 32 serial PUTs. The alternative is parallel PUTs, with startup cutting a file at its first gap, since data past a gap was never flushed. That conflicts with "a gap is a loud error".
2. **Directory rename rule.** "`Info.plist` last" names a Time Machine file. Is "every chunk 0 last" enough as the general rule, with `Info.plist` only as tie-breaker?
3. **Objects without a parent marker** (hand edits, bugs): error at startup, or ignore? The draft never deletes them.
4. **Leftovers on create** cost one LIST per created file. Could the server track failed deletes in memory and skip it?
5. **Case.** S3 keys are case sensitive. Lookups stay exact-case, as the bundle is case-sensitive APFS inside anyway. Does any Time Machine lookup differ in case?
6. **Memory budget** for dirty chunks and the read cache is still open. A large `Truncate` grow is dirty zeros in memory until FLUSH or an early upload.
