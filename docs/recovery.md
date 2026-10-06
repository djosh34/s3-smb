# Recovery

Recovery is built in. When the data folder is missing or older than the newest
database copy in the bucket, s3-smb restores that copy at start, before it
serves. There is no prompt and no recovery command.

## What is in the bucket

- `chunks/<random name>`: file data in pieces of up to 8 MiB. Every upload gets
  a new name that is never reused, and a chunk is never overwritten.
- `db/<sequence>-<commits>-<history>`: full copies of the SQLite database. A
  copy is uploaded at every start and every 15 minutes after, and the 4 newest
  are kept. The highest sequence is the newest.
- `lock/<server ID>/<run ID>`: the bucket lock of each run, renewed every
  minute.

A replaced or deleted chunk goes to a trash table in the database and is
deleted from S3 only once the oldest of the 4 kept copies was taken after it.
So every kept copy can still be restored. A chunk that no copy knows, left by a
crash or a lost data folder, stays in the bucket as waste.

Nothing in the bucket is encrypted by s3-smb. Turn on "Encrypt backups" in Time
Machine, so the chunks hold only encrypted data.

## What to keep outside the machine

- The bucket name, region, endpoint and addressing setting.
- Working S3 credentials for that bucket.
- Any CA certificate, client certificate and client private key you need to
  reach the endpoint.
- The Time Machine encryption password.

## What a lost data folder costs

The data folder holds the database. If it is lost, the next start restores the
newest copy, which is normally at most 15 minutes old and at most 30 minutes
old when copies fail. s3-smb stops with an error when its newest copy is older
than that. Everything after the copy is lost, and the whole share rolls back to
that moment together. Time Machine then sees its newest backup as unfinished
or missing and makes a new one. Earlier backups stay valid.

The same applies when the disk under the data folder does not honour flush and
loses recent writes in a power cut.

## Recover on a new machine

1. Install s3-smb on the new machine with the same config and an empty data
   folder.
2. Run `s3-smb serve`. If the old machine's lock is still in the bucket, the
   new server waits until that lock has not been renewed for 10 minutes, then
   takes over. If the old server is still running somewhere, it keeps the lock
   and the new one keeps waiting.
3. Once it logs `SMB serving`, restore with Time Machine as usual.

If the old machine comes back later with its old data folder, it waits for the
new server's lock in the same way, then restores the newest copy over its own
database before it serves.

## Read a file without s3-smb

Download the newest copy, the `db/` object with the highest sequence, and open
it with `sqlite3`. This query lists the chunks of one file, given its path in
the share:

```sql
WITH RECURSIVE path(id, name) AS (
  SELECT id, name FROM files WHERE parent = 1
  UNION ALL
  SELECT files.id, path.name || '/' || files.name FROM files JOIN path ON files.parent = path.id
)
SELECT chunks.idx, chunks.name, chunks.length
FROM path JOIN chunks ON chunks.file = path.id
WHERE path.name = 'Mac.sparsebundle/Info.plist'
ORDER BY chunks.idx;
```

Chunk `idx` holds the file's bytes from `idx * 8388608`. Only its first
`length` bytes count. A missing index, and anything after the last chunk up to
the file's `size` in the `files` table, reads as zeros. With the AWS CLI:

```sh
sqlite3 -separator ' ' copy.db "$QUERY" | while read -r idx name length; do
  aws s3 cp "s3://my-bucket/chunks/$name" chunk
  head -c "$length" chunk > part
  dd if=part of=file bs=8388608 seek="$idx" conv=notrunc
done
truncate -s "$SIZE" file
```

Restore a whole Time Machine bundle the same way, file by file, then open it on
a Mac.
