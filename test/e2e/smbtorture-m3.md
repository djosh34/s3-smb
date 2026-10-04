# M3 Samba selection

For [M3](https://github.com/djosh34/s3-smb/issues/189) and
[#448](https://github.com/djosh34/s3-smb/issues/448).
The default `smbtorture.allowlist` has no M3 entries yet. The separate
`smbtorture-m3.allowlist` contains candidates, not passing gate entries.
A supported failure stays a blocker. No skip counts as a pass.

## Pin and inventory

`testdata/smbtorture-m3.list` records all 60 individual IDs emitted by
`smbtorture --list` for read, rw, getinfo, setinfo, dir, rename, compound,
compound_find and compound_async. Suite names are not selectors.
Both tools reported `4.17.12-Debian`; `dpkg-query` reported
`2:4.17.12+dfsg-0+deb12u4` for `samba-testsuite` and `smbclient`.
Enumeration used the image built from `test/Dockerfile`, with networking disabled.

There are 34 candidates and 26 exclusions or milestone deferrals. The exclusions
file pairs each exact ID with a reason below. A failing probe cannot move an ID
from candidates to exclusions without a source-backed scope or policy decision.

Primary sources are the Samba
[`samba-4.17.12` tag](https://github.com/samba-team/samba/tree/samba-4.17.12/source4/torture/smb2)
and the project's [M0 contract](../../docs/smb-design.md). The final decisions on
[supported features](https://github.com/djosh34/s3-smb/issues/169#issuecomment-5971874343)
and [valid SMB](https://github.com/djosh34/s3-smb/issues/149#issuecomment-5971168630)
control the scope, not a Samba server's defaults.

## Exclusions and deferrals

| Reason in exclusions file | Source and requirement |
| --- | --- |
| windows-eas-acls-shortnames | [`getinfo.c`, `file_levels` and `torture_smb2_getfinfo_access`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/getinfo.c#L34-L131) demand success for alternate names, compression, Windows EAs and security descriptors. `complex` also creates streams, including directory streams. Named streams are not Windows EA or ACL support. |
| quota-objectid-sector | [`getinfo.c`, `fs_levels` and `torture_smb2_fsinfo`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/getinfo.c#L61-L73) require quota, volume object-ID and sector-size information in addition to the five M0 filesystem classes. |
| quota-sector | [`getinfo.c`, `torture_smb2_qfs_buffercheck`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/getinfo.c#L559-L628) requires classes 6 and 11. A Samba3 target flag skips them but would not prove the unmodified test against this server. |
| windows-eas-shortnames-compression | [`getinfo.c`, `torture_smb2_qfile_buffercheck`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/getinfo.c#L631-L714) creates Windows EAs and expects alternate-name and compression queries to succeed; only NOT_IMPLEMENTED is tolerated. |
| acls | [`getinfo.c`, `torture_smb2_qsec_buffercheck`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/getinfo.c#L717-L774) expects security-descriptor buffer negotiation, not the contract's NOT_SUPPORTED refusal. |
| normalized-case-dialect | [`getinfo.c`, `torture_smb2_fileinfo_normalized`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/getinfo.c#L182-L478) skips an unsupported normalized-name class, then requires case-insensitive lookup, directory streams and a second SMB 3.0.2 connection. The contract is case-sensitive, regular-file streams and SMB 3.1.1 only. |
| position-policy | [`read.c`, `test_read_position`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/read.c#L130-L170) expects position 10 by default, but zero with the Windows flag. M0 uses explicit offsets and no mutable file position. This is a policy mismatch, not an excluded READ failure. |
| samba-private-ioctl | [`read.c`, `test_read_bug14607`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/read.c#L293-L441) requires Samba's private padding-control IOCTL and otherwise skips. |
| windows-eas-shortnames | [`dir.c`, `test_one_file`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/dir.c#L452-L674) uses complex-file Windows EA setup and requires alternate-name information for short-name comparisons. Ordinary directory classes remain candidates. |
| optional-file-index | [`dir.c`, `test_file_index`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/dir.c#L1170-L1281) explicitly skips a zero file index. M0 uses opaque storage cookies; the QUERY_DIRECTORY contract permits ignoring INDEX_SPECIFIED and returns zero indexes. This optional feature cannot pass the fail-on-skip gate. |
| objectids | [`compound.c`, `test_compound_related3`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/compound.c#L376-L444) requires successful FSCTL_CREATE_OR_GET_OBJECT_ID, refused by the contract. `related5` remains a candidate because its first, unrelated IOCTL uses an unbound all-ones FileId and expects FILE_CLOSED; the following CLOSE is related. It does not require a successful object-ID grant. |
| acls-objectids | [`compound.c`, `test_compound_related4`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/compound.c#L446-L563) sets a DACL before testing inherited errors. |
| compound-error-policy | [`compound.c`, `test_compound_related6`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/compound.c#L616-L731) requires successful related READ and CLOSE after an ACCESS_DENIED WRITE. M0's recorded simple dispatch policy propagates an error-severity predecessor to dependent FileId commands. No runtime policy change is proposed here. |
| change-notify | [`compound.c`, related7/8/9 and interim1/2](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/compound.c#L733-L1071) require pending notifications and success or cancellation, while #169 requires immediate NOT_SUPPORTED. The interim tests use the same unsupported notification operation at lines 1740-1880. |
| oplocks | [`compound.c`, `test_compound_break`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/compound.c#L111-L207) requires a batch oplock and break acknowledgment. Classic oplocks receive no grant. |
| m4-streams | [`compound.c`, `test_compound_padding`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/compound.c#L1073-L1301) reads the base and `:foo` stream. Deferred to #449, not excluded as unsupported. |
| m4-parent-sharing | [`rename.c`, parent-directory sharing matrix](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/rename.c#L330-L850) checks delete access and share-delete on an open parent. M3 requires sharing reservations before destructive dispositions; M4 owns the full sharing matrix. These four names are coordinated with #449. |
| position-mode-acls-eas | [`setinfo.c`, `torture_smb2_setinfo`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/setinfo.c#L78-L407) is one indivisible ID. Beyond BASIC, disposition, allocation and EOF, it requires mutable position/mode, ACL mutation and Windows EA mutation. Do not invent individual SET_INFO IDs. |

## Coverage limits

`read.eof`, `read.dir` and `read.access` remain candidates. `rw1` uses two
connections; `rw2` uses two handles on one tree.
[`read_write.c`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/read_write.c)
registers their exact IDs. `rw.invalid` includes boundary offsets and a
Samba-specific maximum-file-size expectation. It remains a candidate until the
owner classifies any observed failure; it has not been excluded to make a run pass.

`dir.sorted` only verifies enumeration: the source says unsorted results are not
an error. `compound_find_close` requests an 8 MiB directory response and creates
5000 files; any server-limit mismatch needs an owner decision, not silent removal.
`compound_async` accepts an INTERNAL_ERROR in the first FLUSH; these tests alone
cannot prove successful durable FLUSH or delayed-S3 async behavior.

No complete pinned getinfo or setinfo test covers only the supported classes.
`getinfo.granted` covers the access mask. The raw real-adapter tests in
`query_info_test.go`, `fs_info_test.go`, `set_info_test.go` and the directory tests
must cover the supported classes, buffer sizes, refusals, timestamp sentinels and
live lengths before M3 closes. This inventory does not claim those dependencies
have landed or that their tests pass on this branch.

## Probe and activation status

No candidates have been activated. Handler/runtime dependency unions require
approval from files-io, files-meta and connection owners. Production edits do not
belong to this issue. `TestSambaM3Interop` calls the sole
`testSambaInterop(t, allowlist, clientCommand)` helper, with a fresh daemon for
each exact candidate. The original extraction came from #348/#433 at
`034617a7351d8c6bda8ff14f28b3041e6c434b02`; no M5 production code was imported.
The test-tools lead approved the explicit client command. M2, M4, M5 and M3
candidate calls use `quit`; the separate `TestSambaM3EmptyRootListing` uses `ls`
with no torture names. A zero command exit does not hide a listing status error.
The explicit invocation is:

```sh
S3_SMB_E2E_ENDPOINT=http://127.0.0.1:PORT \
S3_SMB_E2E_BINARY=/absolute/path/s3-smb-next \
S3_SMB_SAMBA_BINARY=/absolute/path/s3-smb-next \
GORACE=halt_on_error=1 \
go test -race -shuffle=on -v -count=1 -timeout=90m \
  -run '^TestSambaM3Interop$' ./test/e2e
```

Build that executable with `go build -race -tags smbnext`. Samba tools must be
available at the pinned version. This selected test is not part of the default
M2 invocation.

`TestSambaM3EmptyRootListing` lists a fresh empty root before any smbtorture
requests. The external proof also lists a populated directory. Run the empty-root
test separately, so its failure does not hide individual candidate outcomes. An NT_STATUS_NO_SUCH_FILE
result is a reproducible interoperability failure to send to the metadata owner
(51e0018), not a reason to change root/dot-entry policy here. Authentication with
`quit` remains the M2 check and is not directory-listing evidence.

After passing probes and dependency readiness, copy only verified exact IDs to
the central default list and record the checked daemon head, command, shuffle
seed and output. M2's 256-credit decision and M4/M5 staged selections stay intact.

## Checked probe results

The disposable production union was
`6578d39b2ce953037bbaf3728fa27a2bf54a8773`. It merged these owner-approved heads,
in order:

- FLUSH/CREATE/READ/WRITE/runtime: #376 `1f1b5d4a6b5a3f6415c94215e68633b5b358f11f`,
  containing #380 `f0c1353541e291483a5e5fa66d0d90fe38bac37f`,
  #399 `0e5d912b4a22498c8ea522a8b6d16299f750f940` and
  #410 `365c90773d19796e39f24f2220688cc205c1ed3e`.
- QUERY_INFO: #395 `f2680fe98fb4884753daa9a7b633f87261364561`.
- SET_INFO: #398 `4064e5b688a5dc2aa43e741008c3f788ca359e13`.
- QUERY_DIRECTORY: #397 `7252f347f7119dfa8ac8163ed439c903e3e6dc11`.
- Filesystem information: #374 `e137e2ba729fa14d5e40ab3cd88b69a6be4c390a`.
- Base rename/disposition: #404 `1e7f4e35584a1bbfed03a29900cb1e109eace782`.

Conflicts retained the sorted registration union and the exact #398 metadata
fixture superset, with metadata-owner approval. No production repair was made.
The owner-approved test-only correction at
`09bc43de8409cb229a41307f7ba7227f76355ae6` expects NOT_SUPPORTED for filesystem
class 8 when #374 is present, instead of standalone #395's INVALID_INFO_CLASS.
The corrected server package passed race/shuffle, saved seeds, raw in-process
real-adapter tests, vet and pinned lint. Its shuffle seed was
`1791117872778439415`.

The actual race-enabled daemon's SHA256 was
`007ff523c39f50b126ac7358430c3b680c67342541e10867a54f35ad1be80543`.
The selected-list test executable ran inside the pinned Docker image, with an
owned loopback MinIO and `GORACE=halt_on_error=1`. Each candidate got its own
fresh daemon through the sole shared helper. All 34 ran: **20 passed, 14 failed,
none skipped**. The run exited 1. Exact names, Samba seeds, binary hashes and raw
log hashes are in [`testdata/smbtorture-m3.proof`](testdata/smbtorture-m3.proof).
The Go shuffle seed was `1791118396382503198`.

The historical failures remain recorded below. The later source-backed compound
classification follows the table; compatible failures still block activation.

| Exact ID | Observed failure | Routed owner |
| --- | --- | --- |
| smb2.read.dir.dir | FILE_IS_A_DIRECTORY instead of INVALID_DEVICE_REQUEST, read.c:204. | files-io |
| smb2.read.access.access | EXECUTE-only READ gets ACCESS_DENIED instead of success, read.c:283. | files-io |
| smb2.dir.many.many | Daemon data race and exit 66 during file creation. | storage/backup via coordinator |
| smb2.dir.modify.modify | Daemon data race and exit 66 during file creation. | storage/backup via coordinator |
| smb2.dir.sorted.sorted | Daemon data race and exit 66 during file creation. | storage/backup via coordinator |
| smb2.dir.large-files.large-files | Daemon data race and exit 66 during file creation. | storage/backup via coordinator |
| smb2.compound.related5.related5 | NOT_SUPPORTED instead of FILE_CLOSED, compound.c:599. | Mac/file operations (IOCTL) |
| smb2.compound.invalid1.invalid1 | Historical encrypted probe disconnected, while Samba expected INVALID_PARAMETER, compound.c:1493. Later A triage identifies the required GCM first-RELATED binding check. | connection; not a compatible encrypted gate expectation |
| smb2.compound.invalid2.invalid2 | Historical encrypted probe disconnected, while Samba expected first-member success, compound.c:1575. Later A triage identifies the required GCM compound-session binding check. | connection; signed variant is separate |
| smb2.compound.invalid4.invalid4 | Historical probe returned NOT_SUPPORTED instead of Samba's INVALID_PARAMETER, compound.c:1724. MS-SMB2 requires disconnect for unknown opcode 0xff, not either status. | connection; Samba expectation is incompatible |
| smb2.compound_find.compound_find_close.compound_find_close | Daemon data race and exit 66 during file creation. | storage/backup via coordinator |
| smb2.rename.rename_dir_openfile.rename_dir_openfile | Rename succeeds instead of ACCESS_DENIED, rename.c:1038. | file operations |
| smb2.rename.close-full-information.close-full-information | CREATE attributes are 0x80 instead of archive 0x20, rename.c:1469. | file operations |
| smb2.rw.invalid.invalid | READ at INT64_MAX with length 1 gets END_OF_FILE instead of INVALID_PARAMETER, read_write.c:233. This occurs before the test's maximum-file-size policy check. | files-io |

### Current compound disposition

The original 34-name probe remains **20 PASS, 14 FAIL, 0 SKIP**. These later
classifications do not turn a failed command into a pass or activate a name.
The staged selector and historical proof are unchanged pending final selection.

A's minimised diagnosis is recorded in
`/tmp/p3-area-connection-triage/compound/triage.md`. The CLI uses SMB encryption
by default; `encryption.enabled: false` controls S3 at-rest encryption, not SMB.
[MS-SMB2 3.3.5.2.1.1](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/94398d18-48ff-4b6c-a32d-0154b2e88238)
requires disconnect for first-RELATED decrypted requests and unrelated members
whose SessionId differs from the transform session. The observed encrypted
`invalid1` and `invalid2` disconnects therefore must not be repaired by weakening
GCM identity guards. Any compatible signed-plaintext variant needs an explicitly
plaintext fixture and A's raw regression, not rewritten expected values.

[MS-SMB2 3.3.5.2.6](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/853e5234-d5d6-4a18-a2f5-1c792e562947)
requires disconnect without an error response for an unrecognised command.
`invalid4`'s unknown 0xff opcode cannot become a compatible gate by changing the
server to return Samba's INVALID_PARAMETER. A owns the conformance regression
and correction; the earlier NOT_SUPPORTED result remains historical evidence.

The current #363 contract propagates error-severity predecessor status. A's
signed `invalid2` diagnosis ends with USER_SESSION_DELETED under that policy.
[MS-SMB2 3.3.5.2.7.2](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/46dd4182-62d3-4e30-9fe5-e2ec124edca1)
and the recorded project contract control any later amendment. F does not
rewrite that expected value or add a protocol whitelist.

D's checked `448f4c1` adds a plain handle-validating refusal for `related5`, not
an FSCTL implementation. It is not yet a passing F Samba rerun on landed code.
Directory READ/access/boundary checks, CREATE attributes, rename behavior,
empty-root listing and all five actual backup races remain genuine blockers
until their owners' checked fixes land and the corresponding real tests pass.

The five race reports identify a read in `dbMeta.genLog` at `sql.go:1085` and a
write in `baseMeta.Load` at `base.go:733`, called by
`backup.Manager.attempts` at `backup.go:196`. Full stderr reports are retained
with hashes in the proof file. No library or backup patch was authored here.

An earlier informational external run used one daemon and a one-hour backup
interval. It reported 22 passes and 12 failures, including two command timeouts.
Timed-out Docker clients left containers running; only those owned containers
were removed. That run is not gate evidence. It also observed archive/hidden/
system attribute mismatches in `dir.modify`, routed to metadata and file-I/O
owners. The fresh canonical run above supersedes its pass claims. The canonical
fixture's normal two-second backup interval was not weakened to avoid the races.

Empty-root `smbclient -c ls` failed in two separate external fresh roots and in
the separate canonical `TestSambaM3EmptyRootListing`. All reported
`NT_STATUS_NO_SUCH_FILE listing \\*`. The canonical shuffle seed was
`1791118659901920595`; it failed without a skip. Populated-directory listing
succeeded in both external runs. The metadata owner classified the empty-root
failure as a reproduced area-C defect, superseding the provisional root policy.
The combined metadata writer owns its fix and regression. Root/dot-entry policy
is unchanged on this test branch. The informational `dir.modify` attribute and
listing findings need C/B diagnosis; rename/open-file and CREATE attributes belong
to B, and the `related5` IOCTL failure belongs to Mac/B, not QUERY_INFO.

Even the 20 passing candidates remain disabled: this was an approved local probe
union, not a landed dependency stack. Area F must rerun against the final server
union and resolve every compatible failure before activating the M3 gate.

The [combined-PR amendment](https://github.com/djosh34/s3-smb/issues/176#issuecomment-5980023245)
now places this work in area F. Draft #451 predates that amendment and must be
folded into the combined test-tools PR, not merged separately.
