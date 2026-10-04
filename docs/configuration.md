# Configuration

`serve` reads one YAML file. Pass it with `s3-smb -c /path/config.yaml serve` or
`s3-smb serve -c /path/config.yaml`. Without `-c`, the path is
`$XDG_CONFIG_HOME/s3-smb/config.yaml`, or `$HOME/.config/s3-smb/config.yaml`
when that variable is unset. The same rule applies on macOS. A missing file is an
error. `help` and `version` do not read the file.

The file holds one YAML mapping. Unknown or duplicate fields, null values, aliases
and merge keys are errors. The file may be at most 1,048,576 bytes. Nothing is
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

storage:
  state_dir: ./state
  cache_dir: ./cache
  cache_size: "10 GB"

s3:
  bucket: your-existing-bucket
  region: us-east-1
  endpoint: "https://s3.example.com"
  path_style: true
  access_key:
    file: ./secrets/access-key
  secret_key:
    file: ./secrets/secret-key

encryption:
  enabled: true
  passphrase:
    file: ./secrets/encryption-passphrase

backup:
  interval: 1h
  trash_days: 14

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
| `storage.state_dir` | `$XDG_DATA_HOME/s3-smb`, otherwise `$HOME/.local/share/s3-smb` |
| `storage.cache_dir` | `$XDG_CACHE_HOME/s3-smb`, otherwise `$HOME/.cache/s3-smb` |
| `storage.cache_size` | 107,374,182,400 bytes (100 GiB) |
| `s3.bucket` | Required, must exist |
| `s3.region` | `us-east-1`. Set the bucket's real region. |
| `s3.endpoint` | AWS S3 for the region; otherwise an `http://` or `https://` origin |
| `s3.path_style` | Chosen by JuiceFS; `true` forces path style, `false` virtual-host style |
| `s3.access_key`, `s3.secret_key` | Required, one source each |
| `s3.session_token` | Empty |
| `s3.tls` | System CA roots |
| `encryption.enabled` | `true`; then `encryption.passphrase` is required |
| `backup.interval` | `1h`, any positive Go duration such as `30m` |
| `backup.trash_days` | `14`, at most 106751 |
| `logging.format` | `text`, or `json` |
| `logging.level` | `info`, or `debug`, `warn`, `error` |

`storage.cache_dir` is one directory, not a list or glob pattern. Its path must
not contain `:`, `,`, `*`, `?`, `[` or a backslash. This also applies to paths
inherited from `XDG_CACHE_HOME` or the config file's directory. Recovery deletes
only the volume UUID directory under this root. The volume cache and
`storage.state_dir` must be separate directories, with neither inside the other.
Recovery refuses to wipe overlapping directories.

## Sizes

Sizes use decimal units B, KB, MB, GB and TB. 1 MB is 1,000,000 bytes. A number
without a unit is bytes. Fractions such as `"0.5 MB"` are allowed and round up to
whole bytes. Negative sizes, binary units such as `MiB`, scientific notation and
values above 9,223,372,036,854,775,807 bytes are errors. Quote sizes.

`cache_size: 0` turns off the disk and memory block caches. s3-smb still needs
memory for I/O buffers (300 MiB by default) and disk for the SQLite database and
metadata backup staging. The cache does not need to hold the whole dataset.

## Retention

A writable server needs `trash_days` of at least 1 and

```text
2 * backup.interval < backup.trash_days * 24h
```

One metadata backup, with its retries, may take as long as `backup.interval`. If
it has not finished by then, the writer stops. With `trash_days: 1` the interval
must be shorter than 12 hours. Read-only serving does not check these bounds and
accepts `trash_days: 0`. [Recovery](recovery.md#why-retention-matters) explains
why the bound exists.

## Credentials

Each S3 key, and the passphrase when encryption is on, takes exactly one source:

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
and the passphrase must be nonempty and contain no NUL byte. The SMB password is
a plain string in the YAML and is not trimmed. An empty, null or missing SMB
password fails to load.

A `command` runs the program directly, without a shell, in the YAML file's
directory, with s3-smb's environment and privileges and an empty stdin. A path
with a slash is relative to the YAML file. A bare name is looked up in `PATH`.
It must finish within 10 seconds and print at most 65,536 bytes. Secret files and
literal values have the same limit. s3-smb never logs the output or the
arguments. It is not a sandbox, so only use programs you trust.

s3-smb warns, but still starts, when a secret or config file is not mode 0400 or
0600 or has another owner, and when the state or cache directory is not mode 0700
or has another owner. It never changes permissions, and creates new files private.

## S3 endpoint and TLS

`s3.endpoint` is `https://host[:port]` or `http://host[:port]`, without
credentials, path or query. HTTPS always verifies the certificate and host name
and needs TLS 1.2 or later. There is no option to skip verification. Use `http://`
only for a local test server. TLS files with an `http://` endpoint are an error.

`ca_file` adds PEM CA certificates to a copy of the system roots, for s3-smb only.
`client_cert_file` and `client_key_file` must be set together. Each TLS file may
be at most 1,048,576 bytes. Changed files take effect after a restart. For
virtual-host style, DNS and the certificate must cover `bucket.endpoint-host`.

The bucket must exist. The provider must support list, get, put and delete, and
`PutObject` with `If-None-Match: *`. s3-smb uses that header so that a key,
identity or metadata backup is never overwritten. A provider that rejects it
makes startup fail.

## Encryption

Encryption is on by default. `encryption.enabled: false` turns it off for data
and metadata backups. Anyone who can read the bucket can then read your files,
and s3-smb logs a warning at startup. In this mode s3-smb does not read the
passphrase source. The setting is fixed when the dataset is created, and a
different value stops startup. Encryption covers what is in S3. The local SQLite
database, the cache and backup staging files are not encrypted.

## Logging

Logs go to stderr. `logging.format` is `text` or `json`, one JSON object per line.
`--log-format text|json` on the command line overrides it and also applies to
errors in the config file. Prompts go to the terminal (`/dev/tty`), never to the
log. s3-smb removes registered secrets (S3 keys, the SMB password, the passphrase,
the session token and the encryption key) from log lines. It never logs SQL
arguments, xattr values or S3 request bodies.
