# s3-smb

s3-smb is an SMB server that stores its files in an S3 bucket. File data goes
to S3 as immutable chunks of up to 8 MiB. A local SQLite database maps files to
their chunks, and a full copy of it goes to the same bucket every 15 minutes.
It is meant for a Mac user who wants Time Machine backups in S3 without a NAS.
It runs in the foreground as one process.

This version needs a new bucket and a new data folder. It cannot read a bucket,
config or local state from v0.2.0 or earlier.

## Install

```sh
go install github.com/djosh34/s3-smb@latest
export PATH="$PATH:$(go env GOPATH)/bin"
```

You need Go 1.26.3 and a C compiler (on a Mac, the Xcode command line tools).
SQLite is built from bundled source.

## Quick start

Use an existing, empty bucket, and one s3-smb per bucket. Save this as
`~/.config/s3-smb/config.yaml`:

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
```

Put the S3 keys in those two files, next to the config. Then run, in a
terminal:

```sh
s3-smb serve
```

Keep the S3 details somewhere other than this machine, because you need them
to recover. Every setting is in [configuration](docs/configuration.md).
[Backblaze B2](docs/configuration.md#backblaze-b2) needs a few bucket settings.

## Time Machine setup

Turn on "Encrypt backups" for this destination before the first backup, in
System Settings, General, Time Machine. s3-smb stores what Time Machine writes
as it is, so this is what keeps your files private in the bucket.

`tmutil setdestination` needs root, and the terminal needs Full Disk Access in
System Settings, Privacy & Security. These are the steps the end-to-end test
runs on a Mac, with s3-smb on the same Mac and the config above. Replace
`PASSWORD` with the SMB password. In the two URLs, percent-encode characters
such as `@`, `:` or `/`. After `-w`, give the password as it is. The first line
keeps s3-smb's default data folder, `storage.state_dir`, out of the backup.

```sh
sudo tmutil addexclusion -p ~/.local/share/s3-smb
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
- File leases, durable v2 handles and reconnect. A durable handle waits for its
  client as long as the client asks, up to 16 minutes, or 120 seconds if it asks
  for none.
- A full sync from the Mac waits until the data is in S3.
- No byte-range locks, named streams, change notification, directory leases,
  oplocks, hard links or persistent handles. Time Machine needs none of them.

One client at a time: while one client is logged in, or one of its durable
handles waits for a reconnect, the server refuses any other client. A reconnect
from the same SMB client is let in. So a new Mac restoring from the share may
wait until the first Mac's durable handles expire, at most 16 minutes. Mounting
the share in Finder during a backup may be refused.

macOS mounts with a new client GUID after a crash, a reboot, a long sleep or
a failed reconnect, so the same Mac then counts as another client. s3-smb
notices a dead link within about a minute, through TCP keepalive and a limit
on unacknowledged data set on each SMB socket. The old client's durable
opens then wait their 120 seconds, and the Mac gets in once they and their
cleanup have ended, about 3 minutes after the link died. Restarting s3-smb on
the same data folder lets it in at once.
[The SMB server design](docs/smb-design.md) has the details.

## What survives a failure

- The connection drops and s3-smb keeps running: everything the server
  acknowledged is kept.
- s3-smb crashes and the local disk is intact: everything flushed is kept.
  Writes acknowledged but not yet flushed may be lost, like a power cut on a
  local disk. Time Machine treats that backup as failed and the next one
  succeeds.
- The data folder is lost, or the machine with it: the next start restores the
  newest database copy from the bucket. That loses up to 15 minutes of backup,
  or up to 30 minutes when copies fail, and the whole share rolls back to that
  moment together. Earlier Time Machine backups stay valid. The disk under the
  data folder must honour flush, or a power cut can do the same.

A network drop of up to about 30 seconds during normal backup traffic does not
end the backup: the Mac reconnects, gets back the files it had open, and the
same backup finishes. macOS refuses to reconnect if the drop came while it was
creating a file or changing file info and had no answer yet. A
drop longer than macOS's 30-second reconnect window also ends the backup. In
both cases the backup fails visibly, earlier backups stay intact and the next
backup succeeds.

S3 may be slow or unreachable for up to 5 minutes: the backup gets slower but
does not fail. After that the backup fails visibly and the next one succeeds.
An outage longer than about 8 minutes lets the bucket lock expire, and s3-smb
exits with an error, since another server could own the bucket by then. If no
database copy reaches S3 for 30 minutes, s3-smb also exits rather than risk
losing more. Start it again, or let launchd do it.

A failed read, write or sync on the local disk under the data folder, such as
EIO or a full disk, makes s3-smb exit too. After a failed sync the system may
hold the data only in memory, so going on could lose it later. The next start
keeps the local database if it is sound, or restores the newest database copy
from the bucket.

## Restart after a crash on a Mac

Install the checked-in [launchd plist](docs/com.s3-smb.plist) once to keep
s3-smb running while you are logged in. `KeepAlive` restarts it after an exit,
including a crash. There is no service-install command.

First run `s3-smb serve` in a terminal once to check the config, then stop it
with Ctrl-C before loading the job. Copy
`docs/com.s3-smb.plist` to `~/Library/LaunchAgents/com.s3-smb.plist`, creating
that directory if needed. Edit the binary, config, `HOME`, working directory
and log paths to absolute paths for your account. launchd does not expand `~`
or shell variables. Keep the same config and data folder. Create
`~/Library/Logs` if it does not exist, then load the job:

```sh
plutil -lint ~/Library/LaunchAgents/com.s3-smb.plist
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.s3-smb.plist
```

Logs go to the plist's `StandardOutPath` and `StandardErrorPath`, by default
`~/Library/Logs/s3-smb.out.log` and `~/Library/Logs/s3-smb.err.log`. To stop the
job, or before editing its plist, unload it:

```sh
launchctl bootout "gui/$(id -u)" ~/Library/LaunchAgents/com.s3-smb.plist
```

A crash can lose writes still in memory and interrupt the current backup.
Restarting the server does not make that backup complete; start another
backup.

## What is stored and how recovery works

The bucket holds the file data in chunks, the 4 newest copies of the database
and one lock key per run. A killed run's key stays behind. s3-smb does not
encrypt or compress anything; Time Machine's own encryption protects the data.

If the machine running s3-smb is lost, install s3-smb on a new one with the same
config and an empty data folder. The first start waits until the old server's
lock is 10 minutes stale, restores the newest database copy and serves. Then
restore your files with Time Machine as usual. The bucket layout, and how to
read a file without s3-smb, are in [recovery](docs/recovery.md).

## Limits

- One SMB client at a time.
- One s3-smb per bucket. A second server, on any machine, waits until the
  first one's bucket lock is 10 minutes stale. The lock uses no conditional
  writes, so it is not a guarantee: do not start two servers on purpose.
- s3-smb listens on `127.0.0.1:445` unless you set `smb.listen`. It never
  widens the address on its own.
- Time Machine needs a nonempty SMB password stored in the System keychain.
- The bucket grows faster than the bytes Time Machine reports. A write to part
  of a chunk uploads the whole chunk again under a new name, and replaced chunks
  stay until 4 newer database copies exist, about an hour. Unwritten gaps
  inside a chunk are stored as zeros. The share reports at most 1 TiB free, so
  Time Machine uses 268.4 MB bands.
- Writes wait in memory, up to 256 MiB, until the Mac flushes them. The last
  16 chunks read from S3 stay in memory too, up to 128 MiB, so mounting the
  backup image over a slow link needs few requests.
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
[Ported source](docs/vendored.md) describes the code ported from go-smb2.

## Licence

s3-smb's own code is AGPL-3.0-only. The ported code keeps its upstream licence.
See [LICENSE](LICENSE), [NOTICE](NOTICE) and [ported source](docs/vendored.md).
The source of every version is at https://github.com/djosh34/s3-smb under its
tag. If you distribute a modified version, publish its source.
