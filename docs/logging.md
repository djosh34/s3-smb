# Diagnostics

`s3-smb` writes diagnostics through standard Go `log/slog` to stderr. Text and
JSON output support `debug`, `info`, `warn` and `error`. The application installs
text/INFO logging before parsing anything, pre-scans the CLI format override so
configuration failures can be JSON, then applies the effective configuration.
Help/version are command output, not diagnostic JSON. Confirmation/passphrase
prompts must use the controlling terminal, never either diagnostic stream.

`internal/logging` exposes:

- `Install(io.Writer)`: install the process logger and standard `log`/logrus bridges.
- `Configure(format, level string) error`: validate both settings before replacing
  the handler. Empty settings mean text/info. Invalid values are never echoed.
- `RegisterSecret(values ...string)`: register resolved credentials, SMB password,
  passphrase, session token and native private-key material immediately. Empty
  values are ignored. Registered raw, URL-escaped and quoted representations are
  removed at emission, including bound attributes and errors. This is defense in
  depth, **not** a promise to recognize arbitrary transformed secret data.

Never log whole configurations, helper argv/output, passwords, keys, SQL
arguments, xattr contents or SDK HTTP/signing/body traces. Sensitive structured
attribute names are also suppressed. Permission, passwordless and unencrypted
storage warnings retain warning severity. Native progress counters remain usable
but cannot render, including on terminals. There is no syslog stream.

## Intentional native patches (issue #26)

Baseline: JuiceFS v1.4.1 (`0b90c7db5a929ae6adc5faad948d108efd2c99f9`), SMB
`277a9300411249a881a05f7a910f5a83ae3395f2`, and their pinned bundled Xorm source.
Import relocation/source selection are documented separately in packaging docs.

- `internal/juicefs/pkg/utils/logger.go`: each actual logrus handle receives the
  slog formatter at creation; slog owns levels. Native logrus fatal exits with
  status 1 and panic still panics. Both are ERROR diagnostics with `native_level`
  distinguishing fatal/panic; the panic entry is sanitized before unwinding.
- `utils/logger_syslog.go`: do not install native syslog hooks or their direct
  stderr failure paths. `utils/utils_linux.go`: OOM-adjustment errors use the
  normal logger instead of the builtin `println` bypass.
- `utils/progress.go`: always use native quiet/no-output progress, and do not
  replace logging output during construction/completion.
- `internal/smb2/server/log.go`: default native logger is bridged before server
  creation. SMB protocol-owner patches remove xattr WRITE value logging and
  avoid logging the returned list of xattr names in `file_tree.go`.
- `internal/thirdparty/xorm/engine.go` and `log/slog.go`: install the native
  `ContextLogger` implementation before Ping/schema setup, never create a stdout
  logger. Xorm templates retain operation context without interpolating arguments
  (some native cache/schema/conversion messages otherwise dump complete values).
  SQL logging remains disabled; even explicit per-session SQL tracing only emits
  timing, never SQL text/arguments.
- `internal/juicefs/pkg/object/s3.go`: provide the native Smithy logger to every
  SDK configuration load, including region discovery. The explicit application
  S3 constructor also supplies `logging.SDKLogger{}` before client construction.
  Request/body/header/signing debug flags are never enabled.
- Native cache/metadata status (`chunk/{cached_store,disk_cache,mem_cache}.go`,
  `meta/{sql,quota}.go`) uses decimal formatting. Actual capacities are unchanged:
  32 MiB becomes **33.554432 MB**, 100 MiB becomes **104.8576 MB**, not 32/100 MB.
  Byte-count humanization uses decimal `humanize.Bytes`; bandwidth uses 1,000,000
  bits per Mbps. The backup owner removes raw timestamp xattr parse logging in
  `vfs/backup.go` and the recovered-export `debug.PrintStack` bypass in
  `meta/sql.go`: export panic returns/logs a generic failure, never its payload.

## Limits

Normal JSON diagnostics are one valid JSON object per line. An unrecovered native
panic first emits its sanitized JSON diagnostic, then Go's runtime can write its
usual non-JSON stack trace and exit nonzero. Runtime/C crashes, arbitrary runtime
memory dumps and third-party code outside the supported integration are not
promised to be JSON or secret-safe. Logging cannot guarantee delivery to a broken,
full or closed output sink; it does not fall back to an unredacted second stream.
A writer returning an error does not suppress fatal/panic termination. A writer
that blocks can delay synchronous native logging, just as upstream; the
application's separate hard shutdown deadline uses bounded best-effort logging.

## Tests and acceptance boundary

Focused, real local unit/subprocess tests (shared-VM build lock):

```sh
flock /tmp/s3-smb-heavy.lock env GOMAXPROCS=2 go test -p 2 \
  ./internal/logging ./internal/juicefs/pkg/utils \
  ./internal/juicefs/pkg/chunk ./internal/thirdparty/xorm \
  ./internal/thirdparty/xorm/log
```

Tests cover text/JSON and levels, multi-line/quoted synthetic secrets, structured
and late-bound attributes, concurrent JSON framing, global/native fatal and panic
subprocesses, SDK warnings, progress suppression, real SQLite open/constraint
failures, explicit session SQL tracing without arguments, and native decimal
capacity messages. Repeat with `-race` for concurrency checking.

These tests do **not** by themselves prove the Docker SMB-to-MinIO startup/auth/
TLS/object failure matrix, both encryption modes, Linux CI execution or Mac
acceptance. The shared local Docker/CI entry point must execute these same tests
and the real E2E cases, collect both output streams and check synthetic markers.
Do not mark issue #26/release acceptance complete on unit tests alone.
