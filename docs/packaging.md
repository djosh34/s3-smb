# Source packaging and installation

The application module is `github.com/djosh34/s3-smb`. It builds with Go
**1.26.3**, CGo enabled, and an ordinary C compiler/platform SDK. Linux/ARM64 is
the primary development platform. Native Darwin build/install execution is
reserved for the final Mac acceptance gate, after the preceding Linux gates.

```sh
go install github.com/djosh34/s3-smb@<published-version>
```

No workspace, nested module, effective `replace`, vendor directory, consumer
build tags, FUSE headers, FoundationDB client, Ceph or Gluster libraries are
needed. SQLite, Zstandard and LZ4 use portable C sources bundled in pinned Go
dependencies. Standard platform libraries such as libc/pthread are not
third-party installation requirements. `CGO_ENABLED=0` is not supported.

## Exact native source

| Root-module directory | Source |
| --- | --- |
| `internal/juicefs` | JuiceFS v1.4.1, `0b90c7db5a929ae6adc5faad948d108efd2c99f9` |
| `internal/smb2` | macos-fuse-t/go-smb2, `277a9300411249a881a05f7a910f5a83ae3395f2` |
| `internal/thirdparty/xorm` | gitea.com/davies/xorm `v1.0.8-0.20220528043536-552d84d1b34a` |
| `internal/thirdparty/cli` | juicedata/cli/v2 `v2.19.4-0.20230605075551-9c9c5c0dce83` |
| `internal/thirdparty/mpb` | juicedata/mpb/v7 `v7.0.4-0.20231024073412-2b8d31be510b` |
| `internal/thirdparty/lru` | juicedata/golang-lru/v2 `v2.0.8-0.20251126062551-1b321869f904` |

The four customized dependencies are JuiceFS's actual pinned replacements,
now ordinary source packages rather than effective module replacements.
Other replacements used only by excluded backends are not required. Original
upstream `go.mod`/`go.sum` files, where supplied in the snapshot, are preserved
as `UPSTREAM-go.mod.txt`/`UPSTREAM-go.sum.txt` for provenance only. They are not
active modules. The root `go.mod`/`go.sum` are the application's only graph.

[`source-manifest.json`](source-manifest.json) records every selected original
file, source module/version/checksum, original SHA-256, and its SHA-256 after
mechanical import relocation. This baseline deliberately does **not** change
when intentional application patches are applied. New application/regression
files are maintained normally by Git and are not falsely attributed upstream.

Selection preserves native filesystem/chunk/cache/meta/vfs implementations
and their support packages. SQLite is the registered metadata backend. S3,
local file and memory object stores are retained (the latter two support
isolated native tests); unrelated object backends and Redis/TKV/MySQL/Postgres
metadata registrations are excluded. Some shared utilities still reference
Redis types and native sync includes its CLI configuration types; these are
ordinary Go dependencies, not alternative supported application backends.
The server, vfs, stats, protocol/crypto packages and their existing SMB tests
are bundled; the SMB executable, OS-filesystem implementation, bonjour and INI
configuration are not. JuiceFS's service-dependent multi-backend tests are
not copied wholesale; application-owned regression/integration tests cover
supported behavior. Two Windows-only files which import WinFSP or JuiceFS's
Windows/FUSE support are excluded. This is not a Windows product.

## Intentional changes and review

Baseline packaging changes are **only** import-prefix relocation, source
selection, and three `fmt.Fprintf(writer, suggestion)` calls changed to
`fmt.Fprint(writer, suggestion)` in the customized CLI (`app.go`,
`command.go`). The latter fixes Go 1.26 vet's nonconstant-format rejection and
prints the suggestion literally. Packaging does not rewrite SQL, progress,
cache or protocol behavior.

Application patches live alongside native source and must be reviewed against
the contract, with their regression tests:

- metadata SQL/export/protection and `vfs/backup.go`: consistent export,
  success reporting, safe staging, no-overwrite backups and retirement guards;
- object/chunk/fs: S3 options, effective cache capacity, flush/lifecycle and
  storage integration corrections;
- SMB protocol: error propagation, write-through and connection/handle cleanup;
- utils/progress and Xorm/SMB diagnostics: slog routing and redaction.

These categories identify review boundaries, **not** claims that every patch
or acceptance test is complete. To inspect every modified original file and
reconstruct the import-relocated baseline without overwriting patches:

```sh
python3 scripts/source-snapshot.py --output /tmp/s3-smb-pinned-baseline
# Optional offline verification against an already populated module cache:
python3 scripts/source-snapshot.py --cache "$(go env GOMODCACHE)"
# Compare a reported native file to its corresponding baseline with diff -u.
```

The script verifies downloaded module sums and all selected source hashes,
requires an empty reconstruction directory, and lists application-modified
upstream files. It never overwrites the working tree. Compare additions and
all listed files in the actual reviewed Git revision. Hash validation is
source provenance evidence, not behavioral acceptance.

## Linux packaging checks

```sh
flock /tmp/s3-smb-heavy.lock env GOMAXPROCS=2 scripts/check-packaging.sh
# Full local Docker suite: see docs/testing.md.
```

The smoke command uses untagged, non-vendor, non-workspace builds, tests the
source/license restrictions, executes real SQLite/zstd/lz4 CGo roundtrips,
inspects the actual application dependency graph and CGo flags, and performs
a local root build/install plus installed help/version execution. It does
not pretend a local install is `go install ...@version` from a public module.
The final public Linux test must start outside the checkout with fresh caches
and install an actually published candidate. Once the reviewed candidate is
published, `scripts/check-public-install.sh vX.Y.Z-rc.N` creates an empty
working directory and fresh GOPATH/module/build caches, uses the public Go
proxy/checksum service, installs the application and executes help/version.
A local proxy fixture is not that check. Native Darwin compilation is not
run early as a shortcut.

## License and corresponding source

Original project code is **AGPL-3.0-only**, with the complete license in
[`../LICENSE`](../LICENSE). Preserve [`../NOTICE`](../NOTICE), all bundled
upstream license files and SMB's complete `Attributions.txt`. Upstream files
retain their original grants and copyright notices, including Apache-2.0,
AGPL-3.0, MIT, BSD-3-Clause, MPL-2.0 and Unlicense terms. MPL-covered modified
files remain available in source form here.

Source for the exact published tag/commit, including modifications and build
scripts, is available without charge at <https://github.com/djosh34/s3-smb>.
Normal `go mod download` obtains the exact external dependency sources and
licenses identified by the root checksums. This release distributes source
for ordinary `go install`; it does not distribute a separate opaque binary.
If redistributing a binary or a modified network service, provide the complete
corresponding source for **that version**, retain applicable dependency
notices/licenses, and satisfy the AGPL network-source requirement. Pointing
only to an unmodified upstream version is insufficient for modified code.
