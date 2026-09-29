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

## Verification still required

Fixes above are tracked by their regression tests and owner evidence. Final fixed-revision reviewer confirmation, the complete frozen-revision local release suite, identical Linux CI, public installation, documentation/test review and actual hosted-Mac Time Machine acceptance remain separate gates. Do not interpret this disposition record as closing those gates.
