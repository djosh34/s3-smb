# s3-smb

s3-smb is an SMB server that stores its files in an S3 bucket. It embeds JuiceFS
and keeps the filesystem metadata in a local SQLite database, which it backs up
to the same bucket every hour. It is meant for a Mac user who wants Time Machine
backups in S3 without a NAS. It runs in the foreground as one process.

## Install

```sh
go install github.com/djosh34/s3-smb@latest
export PATH="$PATH:$(go env GOPATH)/bin"
```

You need Go 1.26.3 and a C compiler (on a Mac, the Xcode command line tools).
SQLite and the compression libraries are built from bundled source.

## Quick start

Use an existing, empty bucket. Save this as `~/.config/s3-smb/config.yaml`:

```yaml
smb:
  listen: "127.0.0.1:1445"
  username: timemachine
  password: "choose-a-password"
s3:
  bucket: my-time-machine-bucket
  region: eu-west-1
  access_key: {file: ./access-key}
  secret_key: {file: ./secret-key}
encryption:
  passphrase: {file: ./passphrase}
storage:
  compression: zstd
```

Put the S3 keys and an encryption passphrase in those three files, next to the
config. Then run, in a terminal:

```sh
s3-smb serve
```

On the first start s3-smb asks `Initialize a genuinely empty S3 dataset?`. Type
`yes`. Later starts do not ask. Keep the passphrase and the S3 details somewhere
other than this machine, because you need them to recover. Every setting is in
[configuration](docs/configuration.md).

## Time Machine setup

`tmutil setdestination` needs root, and the terminal needs Full Disk Access in
System Settings, Privacy & Security. These are the steps the end-to-end test
runs on a Mac, with s3-smb on the same Mac and the config above. Replace
`PASSWORD` with the SMB password. In the two URLs, percent-encode characters
such as `@`, `:` or `/`. After `-w`, give the password as it is. The first line
keeps s3-smb's default `storage.state_dir` and `storage.cache_dir` out of the
backup.

```sh
sudo tmutil addexclusion -p ~/.local/share/s3-smb ~/.cache/s3-smb
mkdir -p ~/TimeMachineShare
mount_smbfs -N '//timemachine:PASSWORD@127.0.0.1:1445/TimeMachine' ~/TimeMachineShare
sudo tmutil setdestination 'smb://timemachine:PASSWORD@127.0.0.1:1445/TimeMachine'
sudo security add-internet-password -U -s 127.0.0.1 -a timemachine -P 1445 \
  -r 'smb ' -p TimeMachine \
  -T /System/Library/CoreServices/NetAuthAgent.app/Contents/MacOS/NetAuthSysAgent \
  -T /System/Library/CoreServices/TimeMachine/backupd \
  -w 'PASSWORD' /Library/Keychains/System.keychain
tmutil startbackup --block
```

- Use a port other than 445. `tmutil setdestination` fails with exit code 65 for
  an SMB server on `127.0.0.1:445`, also with Apple's own SMB server.
- The SMB account needs a password. s3-smb does not accept an empty one.
- `backupd` mounts the share on its own and needs the password in the System
  keychain. Without that item the backup fails with
  `BACKUP_FAILED_AUTHENTICATION_ERROR (29)`.
- s3-smb must be running whenever Time Machine backs up.

Tested on macOS 15.7.9 (24G830) on Intel, on GitHub's `macos-15` runner image
20260824.0482.1, with s3-smb v0.1.0-rc.8 and MinIO as the S3 server. The test
also restored the backup on a second Mac that had only the bucket.

## What is stored and how recovery works

The bucket holds the file data in blocks, a metadata backup every hour and,
when encryption is on, the encryption key protected by your passphrase. Data and
metadata backups are encrypted by default. The local SQLite database and cache
are not.

If the machine running s3-smb is lost, install s3-smb on a new one with the same
config and an empty state directory. s3-smb finds the newest metadata backup in
the bucket and asks before it recovers from it. Changes after that backup are
lost. Then restore your files with Time Machine as usual. The steps and the
bucket layout are in [recovery](docs/recovery.md).

## Limits

- Only one s3-smb may write a bucket, on any machine. Stop every other one
  first. The state lock only stops a second process with the same
  `storage.state_dir`.
- s3-smb listens on `127.0.0.1:445` unless you set `smb.listen`. It never
  widens the address on its own.
- Time Machine needs a nonempty SMB password stored in the System keychain.
- The bucket grows faster than the bytes Time Machine reports. JuiceFS keeps
  replaced blocks for `backup.trash_days` (default 14) and compaction uploads
  data again, with unwritten gaps filled with zeros. The share reports at most
  1 TiB free, so Time Machine uses 268.4 MB bands and a small first backup takes
  about 1.3 to 1.8 GB. `storage.compression: zstd` shrinks the zeros.
- The S3 provider must support `PutObject` with `If-None-Match: *`.
- s3-smb runs in the foreground. There is no daemon mode or service installer.
- The Time Machine tests kill the application and the Time Machine client
  during a backup. They do not cover power loss or lost S3 objects. A Linux test
  deletes data objects and checks that reading the file fails over SMB.

## Development

`go vet ./... && go test ./...` runs the fast tests. `scripts/test-linux.sh`
runs every test against MinIO in Docker. [Development](docs/development.md)
describes the code, the tests and the Time Machine workflow.
[Vendored source](docs/vendored.md) lists the patches to JuiceFS and the SMB
server.

## Licence

s3-smb's own code is AGPL-3.0-only. The vendored code keeps its upstream licence.
See [LICENSE](LICENSE), [NOTICE](NOTICE) and [vendored source](docs/vendored.md).
The source of every version is at https://github.com/djosh34/s3-smb under its
tag. If you distribute a modified version, publish its source.
