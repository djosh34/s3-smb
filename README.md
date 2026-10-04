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
20260824.0482.1, with s3-smb v0.1.0 and MinIO as the S3 server. The run
installed v0.1.0-rc.9, which is the same commit. The test also restored the
backup on a second Mac that had only the bucket.

## Restart after a crash on a Mac

Install the checked-in [launchd plist](docs/com.s3-smb.plist) once to keep
s3-smb running while you are logged in. `KeepAlive` restarts it after an exit,
including a crash. There is no service-install command.

First run `s3-smb serve` in a terminal and type `yes` to initialize the bucket.
Recovery also needs a foreground run and a typed `yes`: it replaces local
metadata with a backup from S3. launchd has no terminal for either prompt.
Later starts with the existing local database do not ask, including restarts
after a crash.

Stop the foreground process with Ctrl-C before loading the job. Copy
`docs/com.s3-smb.plist` to `~/Library/LaunchAgents/com.s3-smb.plist`, creating
that directory if needed. Edit the binary, config, `HOME`, working directory
and log paths to absolute paths for your account. launchd does not expand `~`
or shell variables. Keep the same config and state directory you initialized. Create
`~/Library/Logs` if it does not exist, then load the job:

```sh
plutil -lint ~/Library/LaunchAgents/com.s3-smb.plist
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.s3-smb.plist
```

Logs go to the plist's `StandardOutPath` and `StandardErrorPath`, by default
`~/Library/Logs/s3-smb.out.log` and `~/Library/Logs/s3-smb.err.log`. To stop the
job, or before editing its plist or recovering metadata, unload it:

```sh
launchctl bootout "gui/$(id -u)" ~/Library/LaunchAgents/com.s3-smb.plist
```

After recovery, stop the foreground process and load the job again. A crash
can lose writes still in memory and interrupt the current backup. Restarting
the server does not make that backup complete; start another backup.

## What is stored and how recovery works

The bucket holds the file data in blocks, a metadata backup every hour and,
when encryption is on, the encryption key protected by your passphrase. Data and
metadata backups are encrypted by default. File data is not compressed. The
local SQLite database and cache are not encrypted.

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
  about 1.3 to 1.8 GB.
- The S3 provider must support `PutObject` with `If-None-Match: *`.
- s3-smb runs in the foreground. There is no daemon mode or service installer.
  On a Mac, the [launchd plist](docs/com.s3-smb.plist) can keep it running.
- The Time Machine tests kill the application and the Time Machine client
  during a backup. They do not cover power loss or lost S3 objects. A Linux test
  deletes data objects and checks that reading the file fails over SMB.

## Development

`scripts/check.sh` runs the same checks locally and in CI, including race tests
and MinIO integration tests in Docker. Use `scripts/check.sh --gate` for phase
and release gates, including fuzz exploration. [Development](docs/development.md)
describes the code, the tests and the Time Machine workflow.
[Vendored source](docs/vendored.md) lists the patches to JuiceFS and the SMB
server.

## Licence

s3-smb's own code is AGPL-3.0-only. The vendored code keeps its upstream licence.
See [LICENSE](LICENSE), [NOTICE](NOTICE) and [vendored source](docs/vendored.md).
The source of every version is at https://github.com/djosh34/s3-smb under its
tag. If you distribute a modified version, publish its source.
