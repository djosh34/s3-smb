# Implementation review dispositions

These are implementation reviews, distinct from the historical plan approval. Findings and their evidence limits are retained verbatim in the linked reports. Passing a focused regression is not completion of the release's Docker/Linux/Mac acceptance sequence.

## Stage 1: packaging, configuration, SMB protocol, logging

Reviewed `f67262104b174ad448c49ed52813c7ccd516990e` against `f408f1ac9472d90ffe67a0c3bdd947d01bb18165`, in an immutable checkout.

### Standards

[Full report](implementation-stage1-standards.md).

- **Reserved SMB MessageId signing bypass — accepted.** Reject the client-reserved ID rather than exempt it from signature verification. The protocol owner added unsigned and tampered reserved-ID wire regressions and retained failing-before evidence.
- No actionable heuristic/style findings. Preserved native implementation conventions were not treated as new-code violations.

### Spec

[Full report](implementation-stage1-spec.md).

- **Reserved MessageId signing bypass — accepted; same fix as Standards.** The spec reviewer independently reproduced successful unsigned TREE_CONNECT over TCP.
- **Signed related-compound session sentinel rejected — accepted.** Resolve inherited session/tree authorization without rewriting bytes covered by the signature. Parse the compound chain before dispatch and retain its correct response-signing boundaries. The protocol owner added real TCP CREATE/WRITE/FLUSH/CLOSE compound coverage and invalid-sentinel negatives.
- **Helper descendants survive WaitDelay — accepted.** Always terminate remaining members of the helper process group when the direct process returns, including WaitDelay failure. The exact shell/child-PID reproducer is now `TestHelperDescendantTerminatedOnWaitDelay`. This tests termination, not reaping an orphan by the host's PID 1.
- The snapshot's decimal-log test failure was a test substring false positive: `32 MB` matched the correctly converted `33.554432 MB`. The assertion was narrowed; no binary value was relabeled as decimal.

The protocol owner also independently found and corrected unauthenticated TREE_CONNECT dispatch, with a failing-before wire regression. This was necessary authentication enforcement, not a request to replace native authentication.

## Stage 2: storage, adapter, metadata protection, lifecycle

Reviewed `abfb2192185b567346889beb5a1ebf758726c969` against the same base, in an immutable checkout.

### Standards

[Full report](implementation-stage2-standards.md).

- **Native batch trash retirement bypasses protection — accepted.** The batch path must use the same guarded native transaction as single-item retirement. Test actual native trash traversal after expiry, not just a callback in isolation.
- **Read-only SID-0 locks survive a crashed authority — accepted.** Add narrow orphan advisory-lock cleanup while the application's exclusive local state lock is held and before serving. Preserve native conflict handling and read-only namespace/content restrictions; do not introduce another lock implementation.
- No standalone heuristic/style findings or broad rewrite recommendations.

### Spec

[Full report](implementation-stage2-spec.md).

- **Read-only SID-0 orphan locks — accepted; same correction as Standards.** A real subprocess crash/reopen regression is required in addition to normal handle-close tests.
- **Known cross-handle Pread freshness gap — accepted and corrected by the adapter/storage owners.** This was explicitly disclosed before the review. A reader opened before another handle extends the file must not return false EOF. Native positional-read length/metadata refresh and its errors are covered by failing-before and fixed-after tests. No parallel adapter data cache was added.

### Independent security/data-loss supplement

[Full report](implementation-stage2-security.md).

- **Batch trash retirement — accepted; same fix.** This reviewer executed the actual native `emptyDir` path and observed metadata retirement with zero protection callbacks. This proves the metadata guard omission, not S3 block deletion.
- No additional concrete cryptographic/bootstrap vulnerability was found in source inspection. That is not a substitute for executed native-parser, MinIO, or full recovery tests.

## Stage 3: Linux acceptance harness, documentation and final deltas

Reviewed immutable candidate `b99973b38f16ae23ec3d1618e15f8966d6b97c63`.

### Standards

[Full report](implementation-stage3-standards.md).

- **Resource-fork READ ignores offsets and EOF — accepted.** The actual signed SMB/MinIO test repeated the stream contents and eventually returned a permission error. Fix bounded native-value reads, slice by the requested offset/length, return EOF correctly, and propagate native errors. Preserve the default absent Apple information stream without hiding I/O failures. The original complete-file E2E remains unchanged; short/offset/EOF/error regressions were added.

### Spec

[Full report](implementation-stage3-spec.md).

- **Resource-fork ranged READ — accepted; same correction as Standards.** Candidate 1 correctly failed both E2E and its release coverage gate (1 of 104 required cases unproven); it was not promoted.
- **Populated old-cache nonuse was not actually tested — accepted.** The earlier zero-cache tests used an unusable cache path, not a populated old native cache. A new MinIO/subprocess test first proves an intact positive cache satisfies reads with remote GET denied, then switches to explicit zero without deleting/changing that cache: denied remote reads must fail; allowed remote reads must verify the full hash. The old cache tree's names, modes, sizes, timestamps and bytes remain unchanged.

### Other reviewed corrections and rejected proposals

- Native file truncation errors and xattr lookup errors must not become successful destructive operations. Resource-fork resizing must preserve the existing prefix; all are covered by concrete failing-before regressions and narrow corrections.
- An initial proposal to subtract an additional hour from accepted backup intervals was withdrawn after full call-path inspection showed native scheduled trash cleanup already adds two hours, covering hourly bucket rounding. Keep native policy and the existing strict `interval + budget < trash_days * 24h` relationship. Only the native arithmetic safety maximum of 106751 days was added, preventing `(24*days+2)*time.Hour` overflow.

### Final authority-path correction

A concrete embedding-path reproducer showed that a literal `?` in a configured state directory was parsed as a SQLite DSN delimiter, opening a different database from the precreated/locked state path. `meta.NewSQLite` now builds an absolute escaped `file:` URI. It explicitly preserves the former effective private-cache behavior; accidentally activating the native CLI's shared-cache default failed the unchanged concurrent export regression and was corrected, not worked around. Native CLI DSN parsing remains unchanged. `TestNewSQLiteLiteralFilesystemPath` checks the actual SQLite database path, effective FULL and real initialization with `?`, `#`, `%` and spaces. Full native metadata/backup race tests and ten repeated path/FULL/concurrent-export runs passed before the final frozen suite.

## Verification still required

Fixes above are tracked by their regression tests and owner evidence. Final fixed-revision reviewer confirmation, the complete frozen-revision local release suite, identical Linux CI, public installation, documentation/test review and actual hosted-Mac Time Machine acceptance remain separate gates. Do not interpret this disposition record as closing those gates.
