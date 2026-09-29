# Independent security/data-loss supplement

**Revision:** `/tmp/s3-smb-review-stage2` HEAD `abfb2192185b567346889beb5a1ebf758726c969`; reviewed `git diff f408f1a...HEAD` and commits `abfb219`, `f672621`, `0856e0b`. Native changes compared with `/tmp/s3-smb-swarm/baseline-imports-only`. Read the contract, CONTEXT, README and issues #21/#22/#24/#25. Source unchanged; no subagents or other reviewers’ findings consulted.

## P2 — Bulk trash retirement bypasses expired protection

**Location:** `internal/juicefs/pkg/meta/sql.go:2834` (`doBatchUnlink`), with destructive operations at **3060–3100**. Actual native cleanup reaches this through `base.go:3225` → `utils.go:338` → `BatchUnlink`.

**Scenario:** protection expires while native trash cleanup is pending (for example, suspension). Unlike individual unlink, batch unlink uses ordinary `m.txn`, not `maintenanceTxn`. It can delete namespace edges, inode records, symlink targets and xattrs without invoking the installed protection callback. The final object-deletion guard cannot undo that SQLite retirement. This is an integration omission around preserved native behavior, not a request to replace native cleanup.

**Executed proof:** external-overlay regression `TestSecurityReviewExpiredTrashBatchRetirement` invokes native `emptyDir` on a trash directory with an expired gate. It fails its preservation assertion: `status=0 removed=1 node_exists=false xattrs=0 protection_checks=0`. This demonstrates unguarded metadata retirement, **not** S3 block deletion.

**Requirement:** issue #24, “Gate actual native retirement/deletion paths and queued work”; `docs/implementation-plan.md`, “Startup, protection and recovery”: guard actual reference retirement/deletion after downtime/suspension.

**Narrow fix:** use the existing maintenance transaction guard for this batch transaction; retain the regression through the actual `emptyDir` path.

## Evidence and limits

Executed (FAIL, expected preservation assertion):

```sh
cd /tmp/s3-smb-review-stage2
flock -w 90 /tmp/s3-smb-heavy.lock env GOMAXPROCS=2 go test -p=2 -overlay=/tmp/s3-smb-security-overlay/overlay.json ./internal/juicefs/pkg/meta -run '^TestSecurityReviewExpiredTrashBatchRetirement$' -count=1 -timeout=30s -v
```

Probe and output: `/tmp/s3-smb-security-overlay/{security_review_test.go,test.log}`. Used Go’s valid `-p=2` spelling; literal `-p2` did not execute the selected package.

Source inspection found no additional concrete bootstrap/key-reuse/encryption/recovery-precedence vulnerability: authenticated PKCS8 profile and costs are checked before native parsing; publication is conditional; entire exports traverse native encryption; startup compares keys/identity and retains current transport authority. These are inspection conclusions, not executed cryptographic or MinIO acceptance evidence. Known pending Pread freshness delta excluded. No Docker, Mac or CI completion claimed.
