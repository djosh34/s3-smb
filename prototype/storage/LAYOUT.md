# Bucket layout (prototype)

Draft for [#516](https://github.com/djosh34/s3-smb/issues/516). Interface: [storage.go](storage.go).

## Keys

| Key | What it is |
|---|---|
| `files/<path>/<index>` | Chunk `<index>` of the file at `<path>`. Raw bytes, 8 MiB, except the last chunk (0 to 8 MiB). |
| `files/<path>/` | Empty marker object: the directory at `<path>` exists. Every directory has one, so empty directories exist. The root has none. |
| `locks/<server id>` | Lock key of one server. Server ID is hostname plus bucket. Body unused, only Last-Modified counts. |

- **Chunk index:** 8 decimal digits, zero padded: `00000000`, `00000001`, ... Keys sort in index order, so `cat dir/*` concatenates in order. 8 digits allow files up to 800 TB.
- **Parsing:** the last segment of a chunk key is always the index, the rest is the path. `files/a/b/00000003` is chunk 3 of file `a/b`. A file inside directory `a/b` named `00000003` would be `files/a/b/00000003/00000000`, so nothing is ambiguous.
- **Data under `files/`** keeps it apart from `locks/`, so a share folder named `locks` is fine.
- **Existence:** a file exists iff its chunk 0 exists. A directory exists iff its marker exists.
- **Size:** last index × 8 MiB + the last chunk's length. **Modified time:** newest chunk Last-Modified. Nothing else is stored.
- **Shape check:** a gap, or a chunk below the last that is not 8 MiB, is ErrCorrupt.

## Time Machine operations and their S3 requests

`c/i` means `files/<path>/<i>`. Writes check the lock first.

| Operation | S3 requests |
|---|---|
| Create file | LIST `files/<path>/` and DELETE any leftovers, then PUT `c/0` (0 bytes). |
| Create directory | PUT `files/<path>/`. |
| Write | None, data goes to memory. A GET `c/i` only to patch a chunk that is not in memory. |
| FLUSH (data) | PUT each dirty chunk of the file. Chunks below the old last chunk in parallel. From the old last chunk upward in index order, so a crash never leaves a gap or a short chunk in the middle. |
| Full FLUSH | The same, for the dirty chunks of every file. Reply after all PUTs succeed. |
| Read | None from memory. Otherwise a ranged GET per chunk touched. |
| List `bands/` | None. Served from memory. |
| Extend (EndOfFile, write past end) | None until FLUSH, then the PUTs above with real zeros. |
| Shrink (incl. EndOfFile 0 on `Info.plist`) | DELETE `c/i` above the new last chunk, top down, then PUT the shortened last chunk (GET it first if not in memory). |
| Delete file | DELETE `c/0`, then the other chunks in parallel. |
| Delete directory | DELETE `files/<path>/` (must be empty). |
| File rename (`.plist.tmp` to `.plist`) | Flush the source. COPY `c/1..N`, COPY `c/0`. DELETE old `c/0`, DELETE old rest. |
| Directory rename (`.incomplete` to `.sparsebundle`) | Flush the files under it. PUT new markers, parents first. COPY every chunk except chunk 0s, then every `c/0`, `Info.plist` last. DELETE old `Info.plist` `c/0`, other old `c/0`s, old chunks, then old markers deepest first. |
| Startup | LIST `locks/`, refuse if another key is younger than 10 min. PUT `locks/<id>`. LIST `locks/` again, on a fresh other key DELETE ours and refuse. DELETE keys older than 10 min. LIST `files/` (all pages). DELETE chunks with no `c/0`. Check every shape. |
| Running | PUT `locks/<id>` every minute. No write after 8 min without a successful renewal. Clean shutdown: DELETE `locks/<id>`. |

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
5. **Case.** S3 keys are case sensitive. Should lookups stay exact-case, as the bundle is case-sensitive APFS inside anyway?
6. **Memory budget** for dirty chunks and the read cache is still open.
7. **Path length.** S3 keys are limited to 1024 bytes. Longer paths get an error.
