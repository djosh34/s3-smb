# s3-smb

s3-smb is an SMB server that stores its files in an S3 bucket. It embeds JuiceFS
and keeps the filesystem metadata in a local SQLite database, which it backs up
to the same bucket every hour. It is meant for a Mac user who wants Time Machine
backups in S3 without a NAS. It runs in the foreground as one process.

v0.2.0 starts on a fresh bucket. It cannot read the buckets, metadata backups or
local state of v0.1.0.

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

Each release is tested on macOS 15 on Intel, on GitHub's `macos-15-intel`
runners, with MinIO as the S3 server. The test also restores the backup on a
second Mac that has only the bucket. The release notes link the run. With the
defaults, macOS 12 and later should work. macOS 11.3 to 11.5 need
`smb.encryption: false`. Older versions cannot connect.

## What the SMB server supports

s3-smb serves one share to one Mac running Time Machine. It supports only what
Time Machine needs:

- SMB 3.1.1 only, with NTLMv2 login for the one configured user. No guest or
  anonymous login and no Kerberos.
- Signing on every session, and AES-GCM encryption by default.
- One disk share and no share list. Connect with a `smb://host:port/share` URL
  or `tmutil setdestination`. There is no Bonjour advertisement.
- File leases, durable v2 handles and reconnect. A durable handle waits 120
  seconds for its client to come back, or what the client asks for, up to 16
  minutes.
- Byte-range locks that never wait, named streams for extended attributes and
  Finder info, and Apple's full sync.
- No change notification, directory leases, oplocks, hard links or persistent
  handles.

One client at a time: while one client is logged in, or one of its durable
handles waits for a reconnect, the server refuses any other client. A reconnect
from the same Mac is let in. So a second Mac recovering from the share may wait
until the first Mac's durable handles expire, at most 16 minutes. Mounting the
share in Finder during a backup may be refused.
[The SMB server design](docs/smb-design.md) has the details.

## What survives a failure

- The connection drops and s3-smb keeps running: everything the server
  acknowledged is kept.
- s3-smb crashes and the local disk is intact: everything flushed is kept.
  Writes acknowledged but not yet flushed may be lost, like a power cut on a
  local disk. Time Machine treats that backup as failed and the next one
  succeeds.
- The machine is lost and you recover on a new one: you get the state of the
  last metadata backup, taken every hour. Changes after it are lost. Earlier
  backups are intact.

A network drop of up to about 30 seconds during normal backup traffic does not
end the backup: the Mac reconnects, gets back the files it had open, and the
same backup finishes. macOS refuses to reconnect if the drop came while it was
creating a file, taking a lock or changing file info and had no answer yet. A
drop longer than macOS's 30-second reconnect window also ends the backup. In
both cases the backup fails visibly, earlier backups stay intact and the next
backup succeeds.

S3 may be slow or unreachable for up to 5 minutes: the backup gets slower but
does not fail. After that the backup fails visibly and the next one succeeds.

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

- One SMB client at a time, as described above.
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
- The Time Machine tests kill the application or the Time Machine client, or
  cut the connection, during a backup. They do not cover power loss or lost S3 objects. A Linux test
  deletes data objects and checks that reading the file fails over SMB.

## Development

`scripts/check.sh` runs the same checks locally and in CI, including race tests
and MinIO integration tests in Docker. `scripts/check.sh --gate` is the release
gate, with full-length outage tests and fuzzing. [Development](docs/development.md)
describes the code, the tests and the Time Machine workflow.
[The SMB server design](docs/smb-design.md) describes the SMB server.
[Vendored source](docs/vendored.md) lists the patches to JuiceFS.

## Licence

s3-smb's own code is AGPL-3.0-only. The vendored code keeps its upstream licence.
See [LICENSE](LICENSE), [NOTICE](NOTICE) and [vendored source](docs/vendored.md).
The source of every version is at https://github.com/djosh34/s3-smb under its
tag. If you distribute a modified version, publish its source.
