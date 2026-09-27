# Readiness audit of the s3-smb implementation plan

Auditor: `PI_MODEL=deepseek/deepseek-v4.1-flash`, `PI_PROVIDER=openrouter`, `PI_REASONING_LEVEL=max`.

Reviewed revision: `fa36915f6a0de6fdb12b1292cc2102a79349071d` in `/home/joshazimullah.linux/work_mounts/s3-time-machine`.

Files read: `docs/implementation-plan.md`, `README.md`, `CONTEXT.md`, `docs/smb-s3-time-machine-handoff.md`, all 30 issues in `/tmp/s3-smb-plan-audit/issues.json`, the four research notes, the pinned JuiceFS copy, the pinned `go-smb2` module cache, and the full JuiceFS v1.4.1 module cache for the command wiring.

Evidence labels used below: "source inspection" means I read the pinned file, "probe" means an executed test recorded in a research note, "inference" means my reasoning from that evidence.

## Verdict

The backlog can be implemented after the two pending user choices are answered and six plan details are corrected. No application code exists, so every claim rests on source inspection and the research probes. The one-writer limit, install layout, logging, cache and recovery scope are concrete enough to build against. The weak spots are the definition of a recoverable backup, the write-through path, the memory cost of the chosen consistent export, and the error statuses the adapter can actually produce. The AGPL-3.0-only choice is settled and does not create a dependency problem in the researched graph.

## Findings

### 1. Passphrase-only recovery still needs the RSA key, so Q2 must be answered as a key-location choice

Severity: high. User decision: required.

The plan says the user asks whether recovery can need "only a passphrase as the encryption secret, without retaining a separate key file" at `docs/implementation-plan.md:84`. That wording hides where the encryption secret lives.

Source inspection of the pinned JuiceFS copy shows the key is unavoidable. `ParsePrivateKeyFromPem` in `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/object/encrypt.go:67` returns `ErrKeyNeedPasswd` when the PEM is encrypted and no passphrase is given, and returns a parse error when no PEM exists. The passphrase only unlocks the PEM. Opening the encrypted object store parses the format's `EncryptKey` with `JFS_RSA_PASSPHRASE` in the full pinned module at `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/juicedata/juicefs@v1.4.1/cmd/format.go:293`. The automatic metadata dump removes the S3 secret key and session token but not `EncryptKey` at `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/meta/sql.go:5044`, and the research note records that the dump is uploaded through the encrypted store and cannot decrypt itself.

Failure scenario: the user keeps only a passphrase, loses the PEM, and recovery fails on every candidate backup, including backups that load fine once the key is present. The passphrase is not a substitute for the key.

Smallest correction: state the options plainly. The private key remains required. The user either retains the PEM as a separate external file, or the app stores a passphrase-protected copy of the PEM as its own object in the bucket under a documented path. The second option removes the separate file from the user's recovery kit, leaving the passphrase plus S3 access credentials plus bucket and volume details. It does not reduce recovery to one secret. The app must upload the key object through the raw client, not the encrypted store, or it will be unreadable.

Recommended answer: choose the stored-in-S3 key copy if the goal is the smallest set of retained items, with a strong passphrase, and document that loss of the bucket or its credentials is unrecoverable. Keep the PEM external if the user wants bucket compromise and a weak passphrase to remain insufficient.

### 2. Credential refresh is about S3 API keys, not TLS certificates

Severity: high. User decision: required.

The plan already draws the distinction at `docs/implementation-plan.md:80`: the question is S3 access credentials, not certificate rotation. The user's confusion is understandable because the same config carries TLS material in the `s3.tls` group at line 56.

S3 access credentials authenticate object-store API calls. Ordinary access keys stay valid until replaced or revoked. Temporary session credentials include an expiry and a session token, and the daemon stops being able to read or write S3 when they lapse. TLS certificates are transport identity and trust material: `ca_file`, `client_cert_file` and `client_key_file`. Startup step 1 loads TLS settings once, and nothing in the plan reloads them while the daemon runs. Replacing a certificate file therefore also requires a restart. It is simply a different concern from rotating an access key.

Failure scenario for the pending choice: the user selects startup-only resolution but also configures short-lived session credentials, for example a one-hour token from a password manager. S3 calls begin failing within the hour, the hourly backup fails, and the fail-stop rule shuts the daemon down. The plan's warning at line 80 does not prevent that outcome.

Smallest correction: answer Q1 with startup-only resolution for the first release, and add one sentence that TLS material is also startup-only. Reject or explicitly warn on session tokens whose expiry cannot outlive a normal run, or defer session-token support to a later release with a refresh policy. Issue 20's acceptance line "Credential rotation tests enforce the final refresh choice" then has a defined meaning.

Recommended answer: resolve at startup and retain until restart. Treat live credential refresh and session-token renewal as out of scope for the first release. Document restart as the rotation action.

### 3. "Newest usable backup" is never defined, and loading is not proof of recoverability

Severity: high. User decision: none, but the plan needs a correction.

Startup step 4 at `docs/implementation-plan.md:106` offers "the newest usable native metadata backup" and treats "no usable backup" as an error. The backup config group says at line 58 that an export existing in S3 "does not promise that every old export still has all its data blocks", and the recovery section says older exports may no longer be recoverable. Issue 6 settled that "a retained metadata file is useless if its referenced objects have been deleted" and "a successful metadata backup must be recoverable, not merely an uploaded file".

Source inspection and the probe show the gap. Replaced slices are retained only for the trash window, and the writable probe recorded that a superseded backup's edited file returned `EIO` after cleanup. Loading a JSON dump into SQLite checks parsing and format, not the existence of every referenced chunk object. A dump can decrypt, decompress, import and publish as "recovered" while the data it points at is gone.

Failure scenario: the daemon is off or failing for longer than the trash window, a provider lifecycle rule removes old chunk objects, or a previous writer cleaned slices after the selected dump was uploaded. The app selects and loads the dump, reports recovery success, and the user later finds unreadable files. The plan's step 4 and issue 24 treat an unloadable or absent backup as the only recovery failures, so a loadable dump with missing blocks would count as success.

Smallest correction: define "usable" as decrypt plus parse plus import plus a data check. A sampled check is a heuristic, not proof: it can catch gross object loss but cannot establish that every referenced object survives. A full guarantee requires walking every chunk mapping in the restored metadata and checking each referenced object exists, at a cost proportional to the slice count. If that is too expensive, the contract and the recovery prompt must say that only metadata load and a sample were verified, so the user is not promised full data availability. Either way, acceptance test 6 must assert that a dump with missing blocks is detected or reported rather than counted as success.

### 4. If the key moves to S3, the PEM protection format must be pinned, and the native helper is weak

Severity: medium. User decision: none, but it feeds the Q2 security answer.

The unapproved proposal says to reuse "maintained native-compatible encoding and crypto" at `docs/implementation-plan.md:86`. The only native key-export helper uses Go's deprecated RFC 1423 PEM encryption. Source inspection of `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs/pkg/object/encrypt.go:56` shows `x509.EncryptPEMBlock(..., x509.PEMCipherAES256)`. The Go 1.26.3 source at `/home/joshazimullah.linux/.local/opt/go1.26.3/src/crypto/x509/pem_decrypt.go` marks that API deprecated and insecure by design, uses a single-iteration MD5 key derivation, and does not authenticate the ciphertext. The same parser accepts PKCS#8 encrypted keys through `pkcs8.ParsePKCS8PrivateKey` at encrypt.go:90, which is the standard place for a modern PBES2 key derivation.

Failure scenario: the passphrase-protected key sits in the bucket, as in finding 1. Anyone who obtains that object can attack the passphrase offline. With the legacy KDF a mediocre passphrase falls quickly, and an unauthenticated format allows silent corruption. The bucket also holds the ciphertext, so a credential leak already exposes data to the same attack.

Smallest correction: if the key is stored in S3, require a standard PKCS#8 encrypted PEM generated with a modern PBES2 KDF, and require the crypto review to pin the parameters. If the key stays external, the legacy format is merely a compatibility default and the risk is lower. Do not generate a new bespoke scheme.

### 5. The write-through flag is forwarded to the adapter but no issue owns honoring it

Severity: medium. User decision: none.

The contract says at `docs/implementation-plan.md:126` that successful FLUSH or write-through means native remote data upload and local metadata synchronization completed. Issue 23 repeats that a successful write-through establishes durability before success. The pinned server does not implement the flag itself; it passes it to the backend. Source inspection of `server/file_tree.go:841` shows `t.fs.Write(..., int(r.Flags()))`, the `vfs.VFSFileSystem.Write` signature takes that integer, and `internal/smb2/const.go:336` defines `SMB2_WRITEFLAG_WRITE_THROUGH = 1`. Issue 22 lists positional write but says nothing about the flag, and issue 23 only says to inspect the path.

Failure scenario: an SMB client sets write-through on a WRITE. The adapter ignores the flag, returns success before the slice upload, and a crash loses a write the client was told was durable. The contract's own guarantee is violated.

Smallest correction: assign the behavior to issue 22 and issue 23 explicitly. `Write` must test the flag and complete the native flush before returning success, or return an unsupported-operation error when it cannot. Add a regression where an injected upload failure after a write-through WRITE produces a failed SMB status. This is source inspection only; no probe covered it.

### 6. The consistent-export requirement has no memory bound

Severity: medium. User decision: none.

The plan rejects the native threshold switch and requires the consistent whole-filesystem SQLite export "for every supported namespace size" at `docs/implementation-plan.md:134`, and issue 24 repeats "always use the verified consistent whole-filesystem SQLite export". The fast JSON path that is consistent builds an in-memory snapshot. Source inspection of `pkg/meta/sql.go:4808` shows `makeSnap` filling maps for every node, symlink, edge, xattr and chunk before the dump begins. The recovery research note warns that forced fast JSON is the smallest change "subject to memory measurements" and asks for export memory and duration measurements. The plan dropped that caveat and defines no supported namespace size.

Failure scenario: a namespace large enough to matter, for example the million-inode range the native code already treats as special, exhausts memory during backup. The process is killed or the backup fails, the fail-stop rule shuts the daemon, and the plan claims support for a size it never measured.

Smallest correction: measure peak memory on the realistic namespace test, then either document a supported inode and chunk maximum or use the single-transaction native binary export for larger namespaces. Add a peak-RSS assertion or a documented budget to issue 24. Inference beyond the recorded source; no probe measured the fast path at scale.

### 7. "Correct protocol statuses" is not implementable in the adapter alone

Severity: medium. User decision: none.

Issue 23 requires "correct protocol statuses for backend, handle, lock and read-only failures". The adapter returns Go errors. The pinned server picks the SMB status. Source inspection of `server/file_tree.go` shows read errors mapped to `STATUS_ACCESS_DENIED` at line 748, write errors mapped to `STATUS_IO_DEVICE_ERROR` at line 846, and open failures mapped to `STATUS_ACCESS_DENIED` unless `os.IsNotExist` matches at line 229. Only locking has a real errno mapping in `server/locking.go:195`.

Failure scenario: a read-only write is rejected as `STATUS_IO_DEVICE_ERROR` rather than an access error, or a remote read failure is reported as `STATUS_ACCESS_DENIED`. The client cannot distinguish a permission decision from infrastructure failure. macOS behavior on a Time Machine volume under such errors is untested.

Smallest correction: state the required status for each error class in the adapter and storage contracts, then patch the narrow server mappings that cannot produce them, or restate the requirement as a failed status rather than a specific one. Add acceptance cases that name the expected status per error class. Source inspection only; no client-level probe.

### 8. The macOS privileged-port rule is specific-address only, so the loopback default needs the privilege but the wildcard bind does not

Severity: medium-low. User decision: none.

The release scope fixes the default bind at `127.0.0.1:445` and promises only to explain permission errors at `docs/implementation-plan.md:15`. The Darwin build and Mac Time Machine test are deferred but expected at line 13 and acceptance item 9 at line 162. My earlier claim that macOS requires root for every port below 1024 was wrong.

Source inspection of current XNU shows the reserved-port privilege check runs only when the requested address is not wildcard. On the main branch and on release tag `xnu-12377.121.6` the guard is at `bsd/netinet/in_pcb.c:990`. The condition is `ntohs(lport) < IPPORT_RESERVED && SIN(nam)->sin_addr.s_addr != 0`. `IPPORT_RESERVED` is 1024 at `bsd/netinet/in.h:278`. The IPv6 path applies the same rule with `!IN6_IS_ADDR_UNSPECIFIED` at `bsd/netinet6/in6_pcb.c:374`. A second gate, `current_task_can_use_restricted_in_port`, runs for every bind but only restricts the ports in `restricted_port_list`. On macOS that list is port 55555 plus debug test entries, so 445 is not restricted. Linux is not an absolute root requirement either: `ip_unprivileged_port_start` defaults to 1024 and can be set to 0 per network namespace, and `CAP_NET_BIND_SERVICE` grants the capability, as documented in `Documentation/networking/ip-sysctl.rst`.

Failure scenario: a normal account runs the default `127.0.0.1:445`. The address is specific, the privilege check applies, and the bind fails with EACCES. An explicit `0.0.0.0:445` bind skips the reserved-port check and works unprivileged when nothing else holds port 445. A wildcard bind can still fail with EADDRINUSE when another process already owns the port, which is port ownership, not privilege.

Smallest correction: state the actual rule in the plan and the Mac checklist. The default loopback bind needs the reserved-port privilege on macOS, or the address must be wildcard. An explicit `0.0.0.0:445` test does not need elevation but depends on port 445 being free and exposes the share beyond loopback. No user decision is needed because both bind forms are already in scope. This is a platform fact from kernel source, not a probe result.

## Minor demonstrated items

- `backup.trash_days` at `docs/implementation-plan.md:58` is a native format property, not a process setting, as shown by `pkg/meta/config.go` and the native config command's use of `m.Init(format, false)`. The plan should state that startup applies config changes to the format and rejects a value the format cannot accept, or the field is not really configurable after first initialization.
- "Add private CA roots to system trust" at line 56 misdescribes the native mechanism. The pinned command wiring sets the HTTP transport's `TLSClientConfig.RootCAs` in process, at `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/juicedata/juicefs@v1.4.1/cmd/format.go:274`. The wording should say the process TLS root pool, not the operating system trust store.
- "Normal bounded retries" at `docs/implementation-plan.md:113` has no retry count, backoff or total bound. Pick numbers so a transient failure and a shutdown decision are deterministic.

## What is ready

The install layout, license handling, logging bridges, cache behavior, one-writer rule, SMB flush-error patch, locking requirement, XDG config rules and credential source rules are backed by the pinned source and the recorded probes. The backlog's ten sub-issues map cleanly to the contract. The Mac and provider compatibility limits are labeled honestly. Nothing in the reviewed evidence contradicts the AGPL-3.0-only decision.

The answer to the focus question is yes, with the two decisions and the corrections above. Issues 19, 20, 21, 22, 23, 24, 25, 26 and 27 can be implemented without guessing product behavior once findings 3 through 7 are folded in. Finding 8 is a Mac checklist wording correction and does not block the Linux milestone. Issue 21 and the recovery documentation wait on Q2. Issue 20's rotation tests wait on Q1.

## Genuine user questions and recommended answers

1. S3 credential refresh. Must credential sources be re-read while the daemon runs, or is restarting after rotation acceptable for the first release? Recommended answer: restart. Resolve credentials and TLS material at startup. Do not claim support for expiring session credentials unless the daemon can refresh them before expiry. This is unrelated to certificate replacement, which is also startup-only.

2. Encryption recovery material. Should the app keep a passphrase-protected copy of the RSA private key in the bucket, or should the user retain the PEM as a separate external file? The private key cannot be removed, and the passphrase only unlocks it. Recommended answer: if the user wants the smallest recovery kit, store a PKCS#8-encrypted key object in the bucket under a documented path, and require the passphrase plus S3 access credentials plus bucket and volume details. Use the external PEM option if the user wants bucket access alone to be insufficient. Either way, update the recovery tests that currently assume an externally restored PEM.
