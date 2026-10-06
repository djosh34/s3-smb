# Configuration

`serve` reads one YAML file. Pass it with `s3-smb -c /path/config.yaml serve` or
`s3-smb serve -c /path/config.yaml`. Without `-c`, the path is
`$XDG_CONFIG_HOME/s3-smb/config.yaml`, or `$HOME/.config/s3-smb/config.yaml`
when that variable is unset. The same rule applies on macOS. A missing file is an
error. `help` and `version` do not read the file.

The file holds one YAML mapping. Unknown or duplicate fields, null values, aliases
and merge keys are errors. An unknown field is named in the error. The file may be at most 1,048,576 bytes. Nothing is
expanded: no environment variables, no `~`, no shell. Relative paths are relative
to the directory of the YAML file.

## Sample

Create the config and secret files after `umask 077`, so only you can read them.

```yaml
smb:
  listen: "127.0.0.1:445"
  share: TimeMachine
  username: backup
  password: "replace-with-your-SMB-password"
  read_only: false
  encryption: true

storage:
  state_dir: ./state
  capacity: "2 TB"

s3:
  bucket: your-existing-bucket
  region: us-east-1
  endpoint: "https://s3.example.com"
  path_style: true
  access_key:
    file: ./secrets/access-key
  secret_key:
    file: ./secrets/secret-key

logging:
  format: text
  level: info
```

## Defaults

| Setting | Default or requirement |
| --- | --- |
| `smb.listen` | `127.0.0.1:445` |
| `smb.share` | `TimeMachine` |
| `smb.username` | Required |
| `smb.password` | Required, nonempty |
| `smb.read_only` | `false` |
| `smb.encryption` | `true`; requires GCM |
| `storage.state_dir` | `$XDG_DATA_HOME/s3-smb`, otherwise `$HOME/.local/share/s3-smb` |
| `storage.capacity` | `0` (report 1 TiB free) |
| `s3.bucket` | Required, must exist |
| `s3.region` | `us-east-1`. Set the bucket's real region. |
| `s3.endpoint` | AWS S3 for the region; otherwise an `http://` or `https://` origin |
| `s3.path_style` | `true` with `s3.endpoint`, otherwise `false`; `true` forces path style, `false` virtual-host style |
| `s3.access_key`, `s3.secret_key` | Required, one source each |
| `s3.session_token` | Empty |
| `s3.tls` | System CA roots |
| `logging.format` | `text`, or `json` |
| `logging.level` | `info`, or `debug`, `warn`, `error` |

## The data folder

`storage.state_dir` is the data folder. It holds the SQLite database that maps
files to their chunks in S3, the server ID and a lock file. The disk under it
must honour flush: a file is flushed only once the database commit is on disk.
If the folder is lost, the next start restores the newest database copy from
S3, which may be up to 30 minutes old. [Recovery](recovery.md) explains what
that means for Time Machine. Keep the folder out of the Time Machine backup.

Only one process can use a data folder at a time. The folder also identifies the
server to the bucket lock: a restart with the same folder takes over the bucket
at once, and a new folder waits until the old server's lock has been silent for
10 minutes.

## Sizes

Sizes use decimal units B, KB, MB, GB and TB. 1 MB is 1,000,000 bytes. A number
without a unit is bytes. Fractions such as `"0.5 MB"` are allowed and round up to
whole bytes. Negative sizes, binary units such as `MiB`, scientific notation and
values above 9,223,372,036,854,775,807 bytes are errors. Quote sizes.

## Capacity

`storage.capacity` is the size the share reports. Free space is the capacity
minus the size of all files. Omitting it or setting it to `0` reports 1 TiB of
free space above what the files use. It does not limit writes, and it is not a
bucket quota: the bucket also holds replaced chunks for a while and the database
copies. Time Machine uses the reported size to decide when to delete old
backups. Restart to change it.

## Credentials

Each S3 key takes exactly one source:

```yaml
access_key: {value: "literal-access-key"}
secret_key: {file: ./secrets/secret-key}
# secret_key: {command: ["/usr/local/bin/my-secret-helper", "s3-secret"]}
```

s3-smb reads each source once at startup. Restart it to pick up a changed file
or a renewed token. A failed source is an error. There is no fallback to another
source or to environment credentials. A session token is a fixed string and must
stay valid while s3-smb runs.

One trailing LF or CRLF is removed from a value. Other whitespace stays. S3 keys
must be nonempty and contain no NUL byte. The SMB password is
a plain string in the YAML and is not trimmed. An empty, null or missing SMB
password fails to load.

A `command` runs the program directly, without a shell, in the YAML file's
directory, with s3-smb's environment and privileges and an empty stdin. A path
with a slash is relative to the YAML file. A bare name is looked up in `PATH`.
It must finish within 10 seconds and print at most 65,536 bytes. Secret files and
literal values have the same limit. s3-smb never logs the output or the
arguments. It is not a sandbox, so only use programs you trust.

s3-smb warns, but still starts, when a secret or config file is not mode 0400 or
0600 or has another owner, and when the data folder is not mode 0700 or has
another owner. It never changes permissions, and creates new files private.

## S3 endpoint and TLS

`s3.endpoint` is `https://host[:port]` or `http://host[:port]`, without
credentials, path or query. HTTPS always verifies the certificate and host name
and needs TLS 1.2 or later. There is no option to skip verification. Use `http://`
only for a local test server. TLS files with an `http://` endpoint are an error.

`ca_file` adds PEM CA certificates to a copy of the system roots, for s3-smb only.
`client_cert_file` and `client_key_file` must be set together. Each TLS file may
be at most 1,048,576 bytes. Changed files take effect after a restart. For
virtual-host style, DNS and the certificate must cover `bucket.endpoint-host`.

The bucket must exist. s3-smb uses only PUT, GET (whole or ranged), DELETE and
LIST. It needs no conditional writes, multipart uploads or versioning. A new
s3-smb version may need a new bucket; there is no migration.

## Backblaze B2

B2 works through its S3 API. Set it up once:

- Make a private bucket with Object Lock off.
- Set its lifecycle rule to "Keep only the last version of the file". B2 keeps
  every deleted or replaced object as a hidden version, and without this rule
  deleted data stays billed forever.
- Make an application key for this bucket only, with read and write access.
- Use the bucket's S3 endpoint and region, for example
  `endpoint: "https://s3.us-west-004.backblazeb2.com"` and
  `region: us-west-004`.

## Encryption

`smb.encryption` requires AES-GCM encryption between client and server by
default. Set it to `false` to allow signed plaintext. Session keys come from the
SMB login.

s3-smb does not encrypt what it stores in S3. Turn on "Encrypt backups" in Time
Machine before the first backup: then every chunk in the bucket holds data that
Time Machine encrypted. The database copies hold file names, sizes and times of
the backup bundle, not file contents.

## Logging

Logs go to stderr. `logging.format` is `text` or `json`, one JSON object per line.
`--log-format text|json` on the command line overrides it and also applies to
errors in the config file. s3-smb removes registered secrets (S3 keys, the SMB
password and the session token) from log lines. It never logs S3 request bodies.
