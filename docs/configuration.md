# Configuration

`serve` reads one YAML file. Select it with `s3-smb -c /path/config.yaml serve`
or `s3-smb serve -c /path/config.yaml`. Without `-c`, the path is
`$XDG_CONFIG_HOME/s3-smb/config.yaml`, or `$HOME/.config/s3-smb/config.yaml`
when the XDG variable is unset. This rule also applies on macOS. A missing file
is an error; there is no configuration wizard. Help/version do not load secrets,
contact S3, or initialize state. CLI handling belongs to the serve command.

The file must contain one mapping/document, with no unknown or duplicate fields,
null values, YAML aliases, or merge keys. The file size limit is 1,048,576 bytes.
There is no environment-variable, tilde, or shell interpolation. Relative file
and directory paths are relative to the YAML file's directory. Nonempty XDG
variables must be absolute paths.

## Sample YAML

Create the configuration and secret files privately (for example, set
`umask 077` before creating them). Replace the example connection details and
provide the referenced files before starting:

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
  # Optional static session token; no automatic refresh:
  # session_token: "your-token"
  # Optional process-only extra CA and paired mTLS credentials:
  # tls:
  #   ca_file: ./certs/ca.pem
  #   client_cert_file: ./certs/client.pem
  #   client_key_file: ./secrets/client.key

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

## Defaults and capacities

| Setting | Default / requirement |
| --- | --- |
| `smb.listen` | `127.0.0.1:445`; explicit other addresses/ports allowed |
| `smb.share` | `TimeMachine` |
| `smb.username` | Required nonempty named account |
| `smb.password` | Required string; explicit `""` enables named-empty access |
| `smb.read_only` | `false` |
| `storage.state_dir` | `$XDG_DATA_HOME/s3-smb`, otherwise `$HOME/.local/share/s3-smb` |
| `storage.cache_dir` | `$XDG_CACHE_HOME/s3-smb`, otherwise `$HOME/.cache/s3-smb` |
| `storage.cache_size` | Omitted: native 107,374,182,400 bytes = 107.3741824 GB |
| `s3.bucket` | Required existing bucket name |
| `s3.region` | `us-east-1` when omitted/empty; set the bucket's actual region explicitly |
| `s3.endpoint` | Omitted/empty: AWS S3 endpoint for the selected region; explicit endpoint is an HTTP(S) origin |
| `s3.path_style` | Omitted: native selection; `true` forces path-style, `false` virtual-host-style |
| `s3.access_key`, `s3.secret_key` | Each independently requires exactly one source |
| `s3.session_token` | Empty/omitted; optional static string |
| `s3.tls` | Verified HTTPS using system roots; optional extra CA and client certificate/key |
| `encryption.enabled` | `true`; passphrase source required when enabled |
| `backup.interval` | `1h`, a positive Go duration (`30m`, `2h`, etc.) |
| `backup.trash_days` | `14`, a nonnegative integer applied to the native volume format |
| `logging.format` | `text`; also `json` |
| `logging.level` | `info`; also `debug`, `warn`, `error` |

Public capacities use decimal units: B, KB, MB, GB, TB; for example **1 MB =
1,000,000 bytes**. An integer without a suffix means bytes. Fractional values
are allowed (`"0.5 MB"`); fractional bytes round upward, so a positive capacity
never turns into zero. Negative sizes, binary suffixes, scientific notation, and
values above 9,223,372,036,854,775,807 bytes are errors. Quote sizes for clarity.

Explicit `cache_size: 0` disables native retained disk and RAM block caches;
omission keeps the same native default **bytes**, not a newly rounded 100 GB
value. Ordinary I/O buffers/readahead, local SQLite/WAL and backup staging still
need memory/disk. A positive cache does not imply the entire remote dataset must
fit locally. Native I/O buffering defaults remain 314,572,800 bytes (314.5728 MB).

Writable protection requires positive trash retention and this strict bound:

```text
backup.interval + total backup operation budget < backup.trash_days * 24h
```

Native cleanup's existing two-hour slack covers its UTC-hour trash-bucket
rounding, so this bound is already conservative. The application does not
subtract another hour or add a grace period or retention policy.

For `trash_days: 1`, the combined interval and total budget must be **strictly
less than 24 hours**. An interval of 23h30m plus a 2-minute total budget is within
that bound. The ordinary defaults (1-hour interval, 2-minute total budget,
14-day trash) remain valid. Runtime protection validates this relationship;
`trash_days: 0` is unsafe for writable serving.

Writable protection also limits `trash_days` to **106751** to avoid overflow in
native cleanup's `time.Duration(24*days+2) * time.Hour` calculation. This maximum
is derived as `floor((MaxInt64/time.Hour - 2) / 24)`, not an arbitrary policy cap.
Native cleanup slack is unchanged. Read-only serving does not construct writable
protection and is exempt from these bounds, including permitting zero trash days.

An old metadata backup does not guarantee its data remains available. Do not use
external S3 lifecycle deletion rules that destroy current data, recovery points,
or encryption bootstrap keys.

## Credentials and helpers

Select one of these mappings separately for each S3 key, and for the encryption
passphrase when enabled:

```yaml
access_key: {value: "literal-access-key"}
secret_key: {file: ./secrets/secret-key}
# Alternative direct argv (not an implicit shell):
# secret_key: {command: ["/usr/local/bin/my-secret-helper", "s3-secret"]}
```

All nine access-key/secret-key source combinations are supported. A selected
source failing is an error, with no fallback to other sources, environment
credentials, or a metadata export. Values resolve once at startup; replacing a
file or renewing helper output takes effect after restart. Session tokens are
static too: temporary credentials must remain valid for the daemon's lifetime.

A file/helper value loses **at most one** final LF or CRLF; the same rule applies
to literal source values. Other whitespace is preserved. Required S3 keys and
passphrases must be nonempty and contain no NUL. SMB passwords are literal
strings, not credential sources, and are not newline-trimmed.

Helpers execute direct argv with empty stdin, the YAML directory as their working
directory, inherited environment, and the daemon's privileges. Executables with
a slash in their relative path resolve relative to that directory; bare names
use ordinary executable PATH lookup. Arguments are not rewritten. There is no
implicit shell, but a user explicitly selecting a shell still runs that program:
this is **not a sandbox**. Trust the YAML and helper programs. Helpers have a
10-second deadline, at most 65,536 bytes each of stdout and stderr, and process-
group termination on cancellation. Secret files/literal values are also limited
to 65,536 bytes. Helper output/argv and underlying error details are never printed.

Existing readable secret/config files with modes other than 0400/0600 or a
different owner emit warnings, not rejection. Existing state/cache directories
warn unless mode 0700 and owned by the effective user. Nothing automatically
chmods or chowns them. Missing/unreadable/nonregular files, invalid credentials,
and failed helpers still fail. Warnings identify the setting, not secret paths
or contents, in either text or JSON logs. Public CA/client certificate files do
not trigger secret-file warnings; the mTLS private key does. The config loader
itself creates no files. State/staging owners create new files privately.

## TLS and transport authority

Custom endpoints must be explicit `https://host[:port]` or intentional
`http://host[:port]` origins, without embedded credentials, query or path. HTTPS
uses verified certificates and hostname checks, with TLS 1.2 as the minimum;
there is no skip-verification option or silent HTTP downgrade. HTTP is useful
for deliberately local MinIO fixtures, but sends traffic without TLS. TLS file
settings with an explicit HTTP endpoint are an error.

`ca_file` appends PEM CA certificates to a copy of the system trust pool used by
the process's S3 TLS client. It does not modify OS trust. `client_cert_file` and
`client_key_file` must be paired and valid. Public TLS files and the client key
are bounded to 1,048,576 bytes each. Replacing local certificate/key files takes
effect on restart. A server certificate renewed under a trusted CA normally
needs no S3 credential change.

Use an existing bucket. The S3 endpoint must support ordinary listing, reads,
writes and deletes, plus conditional `PutObject` with `If-None-Match: *` for
non-overwriting key/identity/metadata publication. A provider that rejects this
operation fails safely; the application does not fall back to an overwriting PUT.

Current validated YAML and its startup credential/TLS snapshot control the S3
destination and transport. Imported metadata must never redirect it or install
old credentials/TLS settings. For forced virtual-host-style addressing, arrange
DNS and certificates for `bucket.endpoint-host`, including in test fixtures.

## Passwordless and encryption opt-out

Use a named account plus an **explicit empty string**, not an omitted/null
password, for passwordless access:

```yaml
smb:
  username: backup
  password: ""
```

This uses the native named-account authentication/signing path, not guest or
anonymous mode. An explicitly wider bind is allowed, with a warning that anyone
who can reach it can access the share. The application never widens its loopback
bind automatically. This setting alone is not proof of compatibility with every
SMB client/signing policy or of successful Time Machine acceptance.

To opt out of application encryption for both objects and remote metadata:

```yaml
encryption:
  enabled: false
```

Disabled mode never resolves even an otherwise configured passphrase source.
Anyone with sufficient S3 read access can then read data and metadata; startup
warns about this. TLS, authentication, metadata protection and local permissions
remain applicable. Encryption mode belongs to the dataset and cannot silently
change on restart/recovery. Enabled encryption protects remote data/metadata,
not native local SQLite/WAL, caches or plaintext backup staging. Recovery still
needs valid S3 access and the remote protected key, plus the passphrase when
application encryption is enabled.

## API and tests

`config.Load(path)` performs strict parsing/defaults/validation only.
`(*Config).Resolve(ctx, logger)` loads a startup snapshot, with native
`*tls.Config` and resolved credential strings. `Storage.CacheSize == nil` means
native default; nonnil zero remains zero. `S3.PathStyle` is likewise a pointer
that distinguishes omitted from explicitly false. CLI logging overrides must be
configured before loading YAML so config errors can also use JSON.

Run the isolated configuration tests with:

```sh
GOMAXPROCS=2 go test -p 2 ./internal/config
```

These tests include real subprocess helpers, lingering-descendant termination,
and local verified/mutual TLS HTTP handshakes. Native S3 credential acceptance
uses the same disposable Docker/MinIO runner as the rest of the Linux suite:

```sh
scripts/test-linux.sh unit ./test/credentials
```

That package tests all nine source combinations with real S3 reads/writes,
startup snapshots, passphrase helper execution/nonexecution, and actual
permissive/differently-owned readable files with text/JSON warnings. It never
uses host credentials; the root test container creates its disposable
foreign-owner fixture. A host-only run without the fixture endpoint explicitly
skips that acceptance test, not marks it as passed.

Neither suite substitutes for SMB authentication, CLI placement/side-effects,
remote encryption/recovery, or final Time Machine gates. Native addressing and
TLS/mTLS acceptance have their separate shared transport suite.
