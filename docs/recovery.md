# Recovery

There are two histories in the bucket. Time Machine keeps its own restore points
inside the sparsebundle it writes to the share. s3-smb keeps metadata backups of
the filesystem that holds that sparsebundle. To get a Mac's files back after
losing the machine that runs s3-smb, you first recover the s3-smb filesystem from
a metadata backup, then restore with Time Machine.

## Terms

- Metadata backup: a consistent snapshot of the SQLite metadata database. It
  holds directory entries, attributes, allocation counters, the map from files
  to data blocks, and pending deletions. It holds no file data.
- Time Machine backup: a backup that Time Machine wrote to the share. s3-smb
  stores it as ordinary file data.
- Recovery point: the filesystem as one metadata backup recorded it. It is usable
  only while the data blocks it points at still exist.
- Writer: the one running s3-smb process that may change a dataset.

## What is in the bucket

All objects are under the prefix `s3-smb/`:

- `s3-smb/format.json`: volume identity and data layout, without credentials.
- `s3-smb/keys/<volume UUID>.pem`: the encryption key, protected by your
  passphrase. Only in encrypted mode.
- `s3-smb/juicefs_uuid`: the volume UUID, which startup checks.
- `s3-smb/chunks/...`: uncompressed file data, encrypted by default, in JuiceFS's layout.
- `s3-smb/meta/snapshot-YYYY-MM-DD-HHMMSS.db.gz`: compressed SQLite snapshots,
  encrypted when encryption is on. The gzip header records the database SHA-256.

The key file is an encrypted PKCS8 private key in PEM form. It uses PBES2 with
AES-256-GCM and scrypt with N=131072, r=8 and p=1, so unlocking it takes about
134 MB of memory. `s3-smb/format.json` holds the same protected key in its
`EncryptKey` field. If the key object is missing, startup stops with
`read protected volume key` and s3-smb does not create a new key. Write the
`EncryptKey` value back to the key object to continue. The passphrase cannot
recreate the key, so the encrypted data is lost only when every copy is gone:
the key object, `format.json` and any copy you keep yourself.

s3-smb takes a metadata backup every `backup.interval` (default one hour) and
at startup. On a normal restart it skips the startup backup if the receipt in
the state directory names a backup younger than `backup.interval` and that
object in S3 still has the recorded SHA-256. After a recovery it always takes a
new one. It keeps every backup from the last 2 days, one per day for 2 weeks,
one per week for 2 months and one per month for 2 years.

## What to keep outside the machine

- The bucket name, region, endpoint and addressing setting.
- Working S3 credentials for that bucket.
- The encryption passphrase, if encryption is on.
- Any CA certificate, client certificate and client private key
  (`client_key_file`) you need to reach the endpoint.

## Recover on a new machine

1. Stop the old writer. If it still runs on another machine, the two will damage
   the dataset. Only one s3-smb may write a bucket, on any machine. The local
   lock only stops a second process with the same `storage.state_dir`.
2. Install the same s3-smb version and write a config with the saved details and
   an empty `storage.state_dir`. Read secrets from a file or a literal value.
   A secret helper program, such as a password manager command, may not work yet
   on a fresh machine.
3. Run `s3-smb serve -c config.yaml` in a terminal. s3-smb finds no local
   database, reads the newest metadata backup and asks:
   `Recover metadata from <backup> (<time>)? ... Continue? [yes/no]`.
4. Answer `yes` only if the old writer has stopped. Changes made after that backup
   are lost.
5. s3-smb checks the snapshot's SHA-256, SQLite integrity and volume identity in
   a staging directory. It uses the current config's bucket, credentials, key
   and trash days, expires and cleans the old sessions, and checks that no lock
   or open-file rows remain. It removes only this volume's cache, then renames
   the staged database into place. A writable start takes a new snapshot before
   serving. A read-only start does not take a backup.
6. Mount the share and check your files. For Time Machine, restore a few files
   with `tmutil restore` or the Time Machine app and compare them.

You may keep `storage.cache_dir` when recovering. Recovery deletes only the
volume UUID directory under that root. Other files and directories stay as they
are.

A valid snapshot does not prove that every data object it points at exists.
A missing object shows up as a read error on that file.

Recovery returns the state of the last metadata backup. Changes after it are
lost and can leave unreferenced objects in S3. There is no garbage-collection
command for those objects. Slice IDs allocated after the snapshot can be reused,
so recovery must wipe the volume cache before publishing the database. If the
process stops between the wipe and rename, the next start runs recovery again.

If recovery fails, do not delete objects, generate a new key, change the
encryption setting or format the bucket to get past the error. s3-smb never falls
back to an older backup on its own. Keep the logs and the bucket as they are.

## Why retention matters

An old metadata backup may point at data blocks that no longer exist. JuiceFS
compacts files: it writes blocks A and B again as a new block C and the current
metadata points at C. A and B go to the trash and are deleted after
`backup.trash_days` (default 14). A metadata backup from before the compaction
still points at A and B. Once they are deleted, that backup can no longer restore
those files.

s3-smb deletes nothing while its newest metadata backup is older than two backup
intervals. If a scheduled backup fails after three attempts, the writer stops.

Do not add S3 lifecycle rules that delete or expire objects under `s3-smb/`. They
bypass this protection and can delete data, metadata backups or the key.
