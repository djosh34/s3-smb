# Passwordless SMB and native zero-cache behavior

Research for [issue #31](https://github.com/djosh34/s3-smb/issues/31). Pins are go-smb2 `277a9300411249a881a05f7a910f5a83ae3395f2` and JuiceFS v1.4.1, `0b90c7db5a929ae6adc5faad948d108efd2c99f9`.

## Answer and smallest integration

Passwordless SMB is possible with the pinned implementation as a **named account with an empty password**. Keep the username, allow an explicitly empty password, and leave both native `AllowGuest` flags false. This uses ordinary NTLMv2 and the normal signed-session path. It needs no replacement authentication stack. True anonymous access is different and does not work through the inspected native NTLM path.

Cache size **0 is a native disabled-cache setting**, not a request for a default-sized RAM cache. Pass zero bytes through to JuiceFS, run its normal configuration checks, and use its native chunk store. Read/write buffers, reader readahead and metadata memory still exist. Zero does not mean zero RAM or no local SQLite/export storage.

Use decimal MB/GB in public settings and display, converting directly to the native byte capacity. No cache algorithm change is needed. Working zero-cache behavior over SMB and MinIO remains a required acceptance test, not a result claimed by this note.

## Passwordless, guest and anonymous are different

The native authenticator registers `UserPassword` entries without rejecting an empty password. Its NTLM server verifies a normal challenge response using that empty password. With `AllowGuest=true`, an unknown username uses the map's empty-string password, but the server still checks its challenge response. It does not accept any arbitrary supplied password. An anonymous exchange with empty username and empty NT response returns `credential is empty`, even with guests enabled.[1][2]

The two `AllowGuest` settings do different jobs:

- `NTLMAuthenticator.AllowGuest` permits unknown usernames through the account lookup.
- `ServerConfig.AllowGuest` marks every successfully authenticated session `SMB2_SESSION_FLAG_IS_GUEST`, even a configured account that supplied a valid nonempty password. The inspected setup does not select guest status from an authentication result, and does not set `IS_NULL`.[1][3]

With both flags false, a named empty-password account gets neither guest nor null status. The server creates normal session signing keys and signs responses. With guest status, it skips that key setup and response signing. Its receive check also exempts guest/null sessions from the ordinary required-signing check.[3][4]

Therefore, do not enable guest merely to allow an empty password. Do not describe the existing guest switch as signed anonymous access. If a separate guest mode is exposed, it must not silently accept a client that requires signing while returning unsigned guest traffic. The named-empty mode avoids that mismatch. An empty password is still public knowledge; a signed session does not turn it into access control against someone who knows the username.

### Executed NTLM probe

A tiny probe used unchanged copies of the pinned NTLM client/server and their internal dependencies. It exercised negotiation, challenge and authentication. Successful cases also checked matching 16-byte session keys and a verified NTLM message integrity code.

| Case | Actual result |
| --- | --- |
| Configured username, configured and supplied password empty, guests off | Accepted; session keys and NTLM integrity check passed |
| Same account, wrong nonempty password | Rejected with `login failure` |
| Unknown username, empty password, guests off | Rejected with `no such user visitor` |
| Unknown username, empty password, guests on | Accepted; session keys and NTLM integrity check passed |
| Unknown username, nonempty password, guests on | Rejected with `login failure` |
| Anonymous client, guests off | Rejected with `credential is empty` |
| Anonymous client, guests on | Rejected with `credential is empty` |
| Configured username/password, guests on | Accepted; session keys and NTLM integrity check passed |

All eight cases passed on Linux/ARM64 with Go 1.26.3. This tested native NTLM, not SMB session flags on the wire, SMB signing negotiation, SPNEGO, a mounted share or any Mac client. The session-flag and SMB-signing statements above are source findings. No Finder guest login or Time Machine compatibility has been demonstrated.

## What zero cache actually does

Source inspection establishes the following at the JuiceFS pin.[5][6][7]

| Setting or component | Native behavior |
| --- | --- |
| `chunk.Config.CacheSize` | Capacity in bytes. `CacheEnabled()` is `CacheSize > 0`. |
| Explicit zero | `SelfCheck` selects `CacheDir="memory"`, disables writeback and block prefetch, and retains capacity zero. |
| Memory cache selected by zero | `newMemStore` receives zero capacity. Its cache insertion returns immediately because `enabled()` is false. This is not a positive-sized RAM block cache. |
| Disk cache at zero | `newCacheManager` returns the memory implementation before expanding or creating disk cache directories. The read path skips cache lookup. Existing disk cache files are not a recovery dependency. |
| Automatic RAM fallback | The 104.8576 MB fallback belongs to positive-cache configurations with unavailable cache directories or lost cache stores. The zero-capacity memory implementation returns false from `isEmpty()`, so the cache-store monitor does not replace it with that fallback. |
| Explicit `CacheDir="memory"` with positive capacity | A retained in-process block cache, unlike capacity zero. |
| Read/write buffers and readahead | Separate from block-cache capacity. `SelfCheck` enforces a buffer minimum of 33.554432 MB; the normal CLI buffer default is 314.5728 MB. Reader readahead is separately configured and is not disabled by setting cache capacity to zero. |

A repeated read on an open handle may still be satisfied by a reader buffer. Do not assert that every SMB READ must become an S3 GET at capacity zero. A restarted daemon with cold buffers is the useful refetch test. The native cache setting also says nothing about SMB-client caching, SQLite's own memory or operating-system buffers.[5][8]

The Go constructor does not supply the CLI's cache-size default. A zero-valued `Config` disables the cache. Preserve the distinction between an omitted application setting and explicit zero before constructing native configuration.

### Decimal settings without changing JuiceFS capacities

JuiceFS's CLI currently defaults `cache-size` to `100G` and `buffer-size` to `300M`. Its parser interprets `M` and `G` as powers of two. The CLI then passes byte values into `chunk.Config`.[9][10]

For the application's required decimal interface:

- `1 MB` means `1,000,000` bytes; `1 GB` means `1,000,000,000` bytes.
- Convert directly to `CacheSize` bytes. Do not first divide by 1,048,576 and truncate. That would turn `1 MB` into zero and accidentally disable the cache.
- Preserve explicit zero. Reject invalid, overflowing or positive sub-byte values rather than rounding a positive capacity to disabled mode.
- If omission inherits the pinned native CLI default, retain its actual `107,374,182,400` bytes, displayed as `107.3741824 GB`, not `100 GB`. The native buffer default is `314.5728 MB`; zero cache does not remove it.

The existing cache diagnostics contain `humanize.IBytes` and fixed binary-unit messages.[5][6][7] Keep the capacities and native behavior unchanged. Application help/status must display decimal units; any exposed native capacity messages covered by the decimal-display requirement need only formatting changes. Never relabel a binary number as decimal.

## Required local Docker + MinIO acceptance

Run these through the actual application container, a real SMB client and a pinned MinIO service on an isolated Docker network. Use test-owned data and fresh client sessions without saved credentials. The same command, fixtures and assertions should run in Linux CI. None of these application tests ran in this research.

1. **Named passwordless access.** Configure a nonempty username and explicit empty password, with native guest flags off. Without an interactive password prompt, connect, write, flush, read, rename and delete. Inspect session setup for neither `IS_GUEST` nor `IS_NULL`. Repeat with client-required signing and verify signed traffic and rejection of a deliberately invalid signature. Wrong nonempty passwords, unknown usernames and anonymous requests must not acquire this account's access. This proves passwordless signed SMB, not Mac compatibility.
2. **Zero versus omitted.** Check that omitted capacity selects the documented native default and explicit zero stays zero through native `SelfCheck`. Verify decimal conversions with `1 MB`, `1 GB` and a positive fractional-MB value below 1 MB. Check help/status display and invalid/overflowing inputs. These are small configuration tests, not large data fixtures.
3. **Zero-cache data round trip.** Start with an empty cache path and zero capacity. Write and flush known data spanning multiple native blocks through SMB. Restart the application with the same SQLite metadata and no cache files, reconnect with a fresh client, and verify hashes. Observe MinIO data-object GETs. Confirm that no retained disk block cache or positive-capacity RAM-cache fallback appeared. Do not use total process RAM or every individual READ as the oracle.
4. **Ignore an old cache at zero.** Populate a cache under a small positive limit, stop, then restart at zero while leaving those files in place. Verify cold reads reach MinIO. In a separate cold run, make MinIO unavailable after successful application startup but before reading an untouched file. The read must fail rather than rely on the leftover disk cache. Restore MinIO and verify a clean restart/refetch. This avoids confusing a startup backup failure with a data-read result.
5. **Disk pressure.** Keep metadata and export staging writable, but make only the cache location read-only or full. Zero-cache operation must still write, flush, restart and refetch successfully without needing that directory. Repeat with a small positive decimal limit to exercise native eviction or fallback, and verify data survives cache loss. Follow native asynchronous capacity behavior rather than inventing an exact instantaneous on-disk byte limit.
6. **Zero-cache write failure.** Interrupt MinIO data uploads during SMB FLUSH at capacity zero. Require an SMB error, not successful local staging. Restore connectivity and verify previously completed data after restarting without cache. This confirms that disabling the cache did not create a writeback-only success path.

If a guest mode is later exposed, add its actual guest-session and mandatory-signing rejection cases separately. Do not use a named empty-password success as evidence for anonymous login. Actual Mac passwordless mounting and Time Machine behavior require their own final runtime acceptance, not an inference from this probe.

## Reproduction and scope

Read the `guest-cache` entry in `/tmp/s3-smb-next-research/issues.json`. Source inspection used these existing directories without changing them:

- `/tmp/s3-time-machine-bundled-research/.research/gopath/pkg/mod/github.com/macos-fuse-t/go-smb2@v0.0.0-20260921092600-277a93004112`
- `/tmp/s3-time-machine-bundled-research/.research/probe/internal/upstream/juicefs`
- `/tmp/s3-time-machine-recovery-research/.research/sources/juicefs`

The last checkout reported the pinned JuiceFS commit. The bundled copy preserves native implementations with import relocation. NTLM probe files and its result log are under `/tmp/s3-smb-next-research/guest-cache-ntlm-probe`. The only external Go package used by that probe was the unchanged cached `golang.org/x/crypto/md4` implementation from v0.49.0, copied privately. No dependency download or application build ran.

```sh
cd /tmp/s3-smb-next-research/guest-cache-ntlm-probe
GOTOOLCHAIN=local GOWORK=off GOPROXY=off GOSUMDB=off \
GOTELEMETRY=off CGO_ENABLED=0 \
GOCACHE=/tmp/s3-smb-next-research/guest-cache-ntlm-probe/gocache \
GOPATH=/tmp/s3-smb-next-research/guest-cache-ntlm-probe/gopath \
GOTMPDIR=/tmp/s3-smb-next-research/guest-cache-ntlm-probe/tmp \
/home/joshazimullah.linux/.local/opt/go1.26.3/bin/go test \
  -p=2 -vet=off ./internal/ntlm -run '^TestPasswordlessResearch$' -count=1 -v
```

Result: PASS, package execution time 0.002 seconds. Cache-zero conclusions are source-verified only. No Docker, MinIO, Mac or Time Machine test ran. No application code, shared-source/cache edits, GitHub changes or subagents were used. Session environment was `PI_MODEL=gpt-6-astra`, `PI_PROVIDER=openai-codex`, `PI_REASONING_LEVEL=xhigh`.

## Pinned sources

[1]: https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/server/authenticator.go#L19-L60
[2]: https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/internal/ntlm/server.go#L233-L326
[3]: https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/server/server.go#L911-L1040
[4]: https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/server/conn.go#L195-L209
[5]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/chunk/cached_store.go#L528-L664
[6]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/chunk/mem_cache.go#L45-L214
[7]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/chunk/disk_cache.go#L1137-L1180
[8]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/vfs/reader.go#L419-L435
[9]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/cmd/flags.go#L196-L252
[10]: https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/utils/humanize.go#L26-L66

Additional exact source paths:

- [Required-signing negotiation](https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/server/server.go#L682) and [guest exemption during receive](https://github.com/macos-fuse-t/go-smb2/blob/277a9300411249a881a05f7a910f5a83ae3395f2/server/conn.go#L473-L503).
- [Zero-cache read path](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/chunk/cached_store.go#L131-L179), [RAM fallback monitor](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/chunk/cached_store.go#L866-L888), and [separate reader buffer/readahead configuration](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/pkg/vfs/reader.go#L709-L725).
- [CLI-to-native byte configuration and SelfCheck](https://github.com/juicedata/juicefs/blob/0b90c7db5a929ae6adc5faad948d108efd2c99f9/cmd/mount.go#L371-L424).
