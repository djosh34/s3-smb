# Stage-2 security finding verification

**Fixed snapshot:** `/tmp/s3-smb-acceptance-candidate1`, HEAD `b99973b38f16ae23ec3d1618e15f8966d6b97c63` (clean working tree).

**Scope:** accepted bulk-trash P2 only. Compared the relevant delta from original reviewed revision `abfb2192185b567346889beb5a1ebf758726c969`; read the original report and external reproducer. Original evidence report preserved unchanged.

## Result: resolved by source inspection; regression retained

- `internal/juicefs/pkg/meta/sql.go:2834` now uses `m.maintenanceTxn` for `doBatchUnlink`. Native batch namespace/inode/symlink/xattr retirement is inside that guarded transaction. The existing helper checks protection on every transaction attempt and again before commit; a failed check returns an error and rolls back.
- `internal/juicefs/pkg/meta/base.go:3213–3217` additionally checks protection in the trash-cleanup loop, preventing repeated retries of a denied directory.
- `internal/juicefs/pkg/meta/trash_protection_test.go:13–57` retains my actual `emptyDir → BatchUnlink` reproducer and strengthens it: requires `EROFS`, zero removals, an invoked gate, preserved inode/xattr, unchanged symlink target and retained directory edge. It tests the previously missed native path, not merely the callback.

This addresses issue #24 and the contract requirement to guard actual native retirement after protection expires. No concrete remaining issue in this accepted finding was identified. No new crypto subsystem changes requested.

## Evidence limits

**Executed previously:** the original revision failed this preservation scenario (`removed=1`, missing inode/xattrs, zero protection checks), recorded in `/tmp/s3-smb-security-overlay/test.log`.

**Fixed revision:** source/diff and checked-in regression inspection only. A nonblocking lock check found `/tmp/s3-smb-heavy.lock` occupied; I did not queue tests or contend with the running local release suite. Therefore this report does not claim an independently executed fixed-revision PASS.

No source modifications. Additional xattr/truncate changes were outside this focused verification. Local release-suite completion, CI, public installation and Mac acceptance are not established by this report.
