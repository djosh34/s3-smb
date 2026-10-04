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
| objectids | [`compound.c`, `test_compound_related3`](https://github.com/samba-team/samba/blob/samba-4.17.12/source4/torture/smb2/compound.c#L376-L444) requires successful FSCTL_CREATE_OR_GET_OBJECT_ID, refused by the contract. `related5` remains a candidate because it tests a closed-handle error, not a successful object-ID grant. |
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
belong to this issue. Staged probes must use a daemon built with
`go build -race -tags smbnext` and the shared runner from #333/#348.

The smbclient directory probe must list a fresh empty root before smbtorture
creates any files, then list a populated directory. An NT_STATUS_NO_SUCH_FILE
result is a reproducible interoperability failure to send to the metadata owner
(51e0018), not a reason to change root/dot-entry policy here. Authentication with
`quit` remains the M2 check and is not directory-listing evidence.

After passing probes and dependency readiness, copy only verified exact IDs to
the central default list and record the checked daemon head, command, shuffle
seed and output. M2's 256-credit decision and M4/M5 staged selections stay intact.
