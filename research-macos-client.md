# What the macOS 15 SMB client (smbfs + Time Machine) sends and requires

Status: DONE (2026-10-03). Read-only research; nothing in the repo was changed.

Sources:
- **[A]** Apple SMBClient-494.120.2 (the macOS 15.6 smbfs), `git clone --depth 1 --branch SMBClient-494.120.2 https://github.com/apple-oss-distributions/SMBClient` into /tmp/smbclient-494. Paths below are relative to that tree. The key constants and logic were checked against SMBClient-538.100.12 (macOS 26) and are the same there.
- **[TMS]** Apple, "Time Machine Over SMB Specification": https://developer.apple.com/library/archive/releasenotes/NetworkingInternetWeb/Time_Machine_SMB_Spec/
- **[S]** Samba master: source3/modules/vfs_fruit.c, docs-xml/manpages/vfs_fruit.8.xml, source3/smbd/smb2_create.c, source3/smbd/durable.c, source3/smbd/avahi_register.c.
- **[MS]** MS-SMB2 3.3.5.9.10: https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/33e6800a-adf5-4221-af27-7e089b9e81d1
- **[R]** The repo at ac712a6: internal/smb2/server/feature.go, file_tree.go:300,514-530, server.go:851, README.md:52-80, and issue #64.

backupd is closed source. Statements about what backupd itself does come from [TMS] or from what the repo observed, and say so.

## 1. Dialects, signing, encryption, preauth

- **Negotiate starts with SMB1 by default.** The default `protocol_vers_map` is 7, so the first packet is an SMB1 NEGOTIATE offering "NT LM 0.12", "SMB 2.002" and "SMB 2.???" (lib/smb/preference.c:900; nsmb.conf.5:94; kernel/netsmb/smb_smb_2.c:3756-3790; smb_smb.c:122-126).
  - If the server answers with an SMB2 NEGOTIATE response for dialect 0x02FF, the client sends an SMB2 NEGOTIATE offering 0x0202, 0x0210, 0x0300, 0x0302 and 0x0311 (smb_smb_2.c:2215-2235, 6670-6720).
  - A 0x0202 or 0x0210 answer to the SMB1 negotiate skips straight to session setup. Do not do that. Answer 0x02FF, then pick 0x0311.
- **Reconnect negotiate.** On reconnect the client sends an SMB2-only NEGOTIATE with MessageId 0. It offers exactly one dialect, the one negotiated before, and the cipher and signing contexts contain only the algorithm already in use (smb_smb_2.c:2253-2316, 806-820, 966-981; smb_iod.c:3460-3465).
- **3.1.1 negotiate contexts sent** (smb_smb_2.c:776-1035):
  - PREAUTH_INTEGRITY: SHA-512, 32-byte salt.
  - ENCRYPTION: AES-256-GCM, AES-256-CCM, AES-128-GCM, AES-128-CCM.
  - COMPRESSION: NONE only (compression is off by default).
  - SIGNING: AES-GMAC listed first, then AES-CMAC. Both are enabled by default.
  - NETNAME.
- **What the client checks in the response contexts.** An ENCRYPTION context must carry exactly one cipher. A SIGNING context must carry CMAC or GMAC (smb_smb_2.c:7078-7200).
- **Client capabilities:** DFS, LEASING, LARGE_MTU, PERSISTENT_HANDLES, DIRECTORY_LEASING and ENCRYPTION, plus MULTI_CHANNEL because `mc_on` defaults to yes (smb_smb_2.c:2184-2195).
- **Signing.** The client's `signing_required` defaults to no.
  - It signs every request only if the server sets SIGNING_REQUIRED in its SecurityMode (smb_smb_2.c:6636-6646).
  - On 3.1.1 it always signs TREE_CONNECT, to finish the preauth check (smb_smb_2.c:10142-10149).
  - FSCTL_VALIDATE_NEGOTIATE_INFO is always signed, but it is sent only for 3.0 and 3.0.2, never for 3.1.1 (smbfs_vfsops.c:1168-1180; smb2fs_smb_validate_neg_info, smbfs_smb_2.c:10740).
  - [TMS] requires "SMB 3.x signing". The repo sets SIGNING_REQUIRED (server.go:851).
- **Encryption** is not requested by default (`force_sess_encrypt` and `force_share_encrypt` are no). The server can leave encryption out by never setting ENCRYPT_DATA.

## 2. Authentication

- The client reads the SPNEGO negTokenInit from the NEGOTIATE response.
  - It tries Kerberos first only if the token lists a krb5, MS-krb5, U2U or PKU2U OID (lib/smb/gss.c:163-182; lib/smb/ctx.c:1764-1852). Otherwise it uses NTLMSSP. The minimum is NTLMv2 (preference.c:881).
  - An empty token makes it fall back to raw NTLMSSP (ctx.c:756-762). To avoid that path, send a negTokenInit that lists only NTLMSSP (1.3.6.1.4.1.311.2.2.10).
- Kerberos is not needed for a single local user.
- Guest sessions turn signing off on the client (smb_gss.c:950-955), so a TM account needs a real password. This matches README.md:77.
- On reconnect the client sends the old session id as PreviousSessionId (smb_conn.c:321; smb_smb_2.c:2438).

## 3. The AAPL create context

- **Request.** The AAPL context is sent first in the create, on the share root. It asks for SERVER_CAPS, VOLUME_CAPS and MODEL_INFO. The client advertises READ_DIR_ATTR, OSX_COPYFILE, UNIX_BASED, NFS_ACE, READ_DIR_ATTR_V2 and HIFI (smb_smb_2.c:195-283; smb_2.h:91-300).
- **The bit that matters for TM is volume cap kAAPL_SUPPORTS_FULL_SYNC (0x4).**
  - It sets kSMBFullFSyncSupported in the TM settings fsctl (smbfs_vnops.c:10298-10303).
  - It turns F_FULLFSYNC on, which would otherwise fail with ENOTSUP (smbfs_vnops.c:10441-10446).
  - [TMS] calls it required. Server caps "are not necessary"; reply 0 if asked.
- **A server that answers AAPL is treated as an OS X server (SMBV_OSX_SERVER).**
  - After any reconnect the client re-sends the AAPL query on the root and fails the reconnect if AAPL is no longer answered (smbfs_node.c:3820-3866). So AAPL must be answered on every connection, not only the first.
  - The client also starts an Apple "svrmsg" CHANGE_NOTIFY on FileId 0xFFFF…FFFF (smbfs_smb_2.c:6921-6926). An error reply is fine.
- **What Samba fruit sets** (vfs_fruit.c:771-912):
  - Server caps: UNIX_BASED always. READ_DIR_ATTR only if the client asks and the share has streams. OSX_COPYFILE only with `fruit:copyfile=yes`, which defaults to no. NFS_ACE if nfs_aces is on.
  - Volume caps: CASE_SENSITIVE only with `case sensitive=yes`. FULL_SYNC iff `fruit:time machine=yes`.
  - Model info: the `fruit:model` string, default "MacSamba".
  - `time machine=yes` also forces `durable handles=yes`, `kernel oplocks=no`, `kernel share modes=no` and `posix locking=no`, and warns unless `strict sync` is on (vfs_fruit.c:1361-1371).
  - With AAPL negotiated, a FILE_OPEN of a 0-byte named stream returns OBJECT_NAME_NOT_FOUND (vfs_fruit.c:4474-4482). This is a compatibility rule a new server should copy.
- **The repo today** sends server caps READDIR_ATTR, UNIX_BASED and NFS_ACE, and volume caps FULL_SYNC (file_tree.go:520-525).
- **READ_DIR_ATTR is optional.** Without it, enumeration falls back to plain FILE_ID_BOTH_DIR_INFORMATION. With it, the server must overload the EaSize and ShortName fields with max-access, resource-fork length and compressed FinderInfo (smb_2.h:236-290).

## 4. Named streams and xattrs

- Streams are on (`SMBFS_MNT_STREAMS_ON`) if the server's FileFsAttributeInformation has FILE_NAMED_STREAMS (smbfs_vfsops.c:1574-1578).
  - xattrs become streams named `name:$DATA`. FinderInfo is the `AFP_AfpInfo` stream and the resource fork is `AFP_Resource` (smb.h:1128-1131).
  - The volume then advertises VOL_CAP_INT_NAMEDSTREAMS and VOL_CAP_INT_EXTENDED_ATTR (smbfs_vfsops.c:2429-2430).
- Without FILE_NAMED_STREAMS those caps are missing. XNU then keeps xattrs and FinderInfo in AppleDouble `._` files (generic VFS behaviour), and readdirattr is disabled (smbfs_vfsops.c:2400-2425).
- Neither [TMS] nor the client source makes streams a TM requirement. No proof was found either way that TM works on a stream-less share. Every working Samba TM setup uses `streams_xattr`.
- The client does not request a file lease on xattr streams (smbfs_smb_2.c:6337-6343). Samba never grants durable handles on streams (durable.c:86-92).
- Which xattrs TM itself writes was not observed in this research. The repo has no capture of stream names (low confidence on details).

## 5. Durable handles, leases, reconnect

### Requests
- **DH and lease requests depend on the server's LEASING capability.** Every regular-file open asks for a durable handle plus a lease, but only if the server advertises GLOBAL_CAP_LEASING (smbfs_node.c:4690-4712; smbfs_smb_2.c:9104-9133).
  - Requested lease state: H always, plus R if the open has read access, plus W if it has write access (smbfs_node.c:5367-5383). A read-write open asks for RWH.
  - Directory leases are requested only with DIRECTORY_LEASING (smbfs_smb_2.c:4356).
- **DH2Q and lease V2 are assumed on every SMB 3.x mount.** smbfs sets SMBV_HAS_DUR_HNDL_V2 for any SMB 3.x mount, and also when the server advertises PERSISTENT_HANDLES (smbfs_vfsops.c:1730-1759). So TM mounts send DH2Q with lease V2 (RqLs, 52 bytes).
  - The DH2Q Timeout is 0 unless backupd set one; the value is in ms (smb_smb_2.c:430-475, 640-700).
  - Persistent handles are requested only if the share has CA and the server has PERSISTENT_HANDLES (smbfs_node.c:4706-4712).
- **TM settings fsctl** (smbfs_vnops.c:10267-10420):
  - kReadSettings reports DurableHandleV2Supported. Because the flag is already assumed, the probe is skipped and the reported timeout is 0.
  - kWriteSettings lets backupd set a DH v2 timeout of up to 16 min. It then re-probes: CREATE `.com.apple.timemachine.supported-<uuid>` with FILE_OPEN_IF, RqLs V2 (RH, new key) and DH2Q, then CLOSE and delete (smbfs_smb_2.c:6953-7110). It fails with E2BIG if the granted timeout differs from the requested one, and with ENOTSUP if no DH2Q is granted. The same call can disable reconnect and set IP QoS to 0x20.
  - [TMS] describes the probe with Timeout 0, then 30 s, then values between 30 and 180 s, and says the server "must correctly honor the Timeout field".
- **Server side** ([MS] plus Samba smb2_create.c:1482-1489, 1902-1928):
  - Grant durability only if the lease holds H (or a batch oplock, or persistent on a CA share).
  - Response Timeout = min(requested, 300 s). For a 0 request use an implementation value; Samba uses 60 s.
  - Track the CreateGuid. A duplicate without the REPLAY flag returns STATUS_DUPLICATE_OBJECTID.

### Reconnect path ([A])
- **Detecting a dead connection.** A synchronous request is declared dead when nothing at all has been received for `max_resp_timeout`: 35 s, or 45 s if leasing was negotiated (smb_iod.c:3012-3045; smb_smb_2.c:6818-6825).
  - An ECHO is sent after 10 s of silence (SMBUETIMEOUT).
  - A single synchronous request times out after 120 s (SMB_SEND_WAIT_TIMO).
  - Requests that got an interim STATUS_PENDING never time out (smb_iod.c:3003-3009). A slow FLUSH or WRITE to S3 should go async, with correct credits.
- **Reconnect loop** (smb_iod.c:3330-3640):
  - Overall window 600 s (SMBM_RECONNECT_WAIT_TIME). TCP retries come at most 5 s apart.
  - **The TM-mount dead timer is 30 s from the start of the reconnect.** After that the share is declared dead, which force-unmounts it (smb_conn.h:781; smbfs_vfsops.c:1691-1699; smb_iod.c:3203-3215).
  - TM mounts give up immediately if a non-idempotent request was in flight (smb_iod.c:123-215, 3349-3357). Those are a CREATE without its CLOSE, LOCK, SET_INFO, and the set-reparse IOCTL.
  - After NEGOTIATE and SESSION_SETUP the client needs at least 5 credits granted (smbfs_node.c:3780-3787). It redoes TREE_CONNECT (a failure ends a TM reconnect) and the AAPL check.
- **Reopening files** (smbfs_node.c:4120-4260, 3554-3740):
  - Each open FID is reopened with a CREATE that carries only DH2C (persistent+volatile FileId and CreateGuid) plus RqLs with the same key and state. There is no MxAc and the disposition is FILE_OPEN.
  - On a TM mount, any failed reopen fails the whole reconnect.
  - The only fallback is a plain reopen, and only for a "sharedFID" with no byte-range locks that never got a durable handle.
  - O_EXLOCK/O_SHLOCK opens ("lockFID") and FIDs that hold byte-range locks have no fallback.
- **Observed.** Issue #64 reports that the client "could not reconnect the share afterwards, because the server has no durable handles". The result was BACKUP_FAILED_DISCONNECTED_NETWORK (26). This matches the source.
- **Samba in practice.** The jamesyc/TimeCapsuleSMB #294 report on Samba describes backups that "only recover via durable reconnect" (anecdotal).
- **Samba's limits** (durable.c:36-100):
  - No durable handles with kernel oplocks, kernel share modes, or POSIX locks held.
  - Regular files only (directories only as persistent). No streams.
  - Default timeout 60 s, maximum 300 s.

### Can TM start without durable handles?
Empirically yes. The repo does not advertise LEASING (feature.go:12-16), so the client never sends DH2Q, yet the README flow and the Mac acceptance runs back up successfully. A Synology forum thread ties "network backup disk does not support the required capabilities" to durable handles being off. That error may come from kWriteSettings. Medium confidence.

## 6. Other protocol details

- **Credits.** The client asks for 256 credits per request until its balance reaches 65535 (smb_2.h:45-50; smb_rq_2.c:270-275, 479-487).
  - CreditCharge = ceil(len/64 KiB). Below 10 credits it drops to 64 KiB single-credit I/O (smb_rq_2.c:165-206).
  - The server must never let the balance reach 0.
- **I/O sizes (LARGE_MTU).** Adaptive quanta of 256 KiB, 512 KiB or 1 MiB, with 8 in flight; the start is 512 KiB × 8 (smbclient_internal.h:112-132; smb_conn.c:798-821).
  - Sizes are capped by the server's MaxRead/MaxWrite and 8 MiB (smb_smb_2.c:59-60, 10770-10810).
  - Without LARGE_MTU it uses MaxRead/MaxWrite with 16 in flight.
  - Expect 1 MiB WRITEs with CreditCharge 16.
- **Compounds** are on by default (preference.c:876).
  - Patterns: CREATE+QUERY_INFO+CLOSE, CREATE+SET_INFO+CLOSE (including delete), CREATE+READ+CLOSE, CREATE+WRITE+CLOSE, FLUSH+CLOSE, CREATE+QUERY_DIRECTORY, and CREATE+IOCTL+CLOSE (smbfs_smb_2.c:472-6687).
  - Up to 10 async compound queries fill the directory cache.
- **FLUSH and F_FULLFSYNC.** fsync sends a normal FLUSH. F_FULLFSYNC sends a normal flush of dirty data first, then one FLUSH with Reserved1=0xFFFF on any open FID (smbfs_vnops.c:10441-10483; smbfs_smb.c:2994-3015; smb_smb_2.c:2068-2075). [TMS] says the server must reach stable storage before it replies.
- **Locks.**
  - O_EXLOCK maps to deny read+write and O_SHLOCK to deny write, as CREATE share modes (smbfs_vnops.c:1026-1170, 1748). DiskImages relies on these.
  - flock() becomes an SMB2 LOCK, always with FAIL_IMMEDIATELY, so there are no blocking locks (smb_smb_2.c:3210-3227).
  - POSIX fcntl locks are not sent over SMB2; they return the local advlock error (smbfs_vnops.c:10795-10800).
- **QUERY_DIRECTORY** uses FileIdBothDirectoryInformation (37), or FileIdFullDirectoryInformation (38) in a few cases (smbfs_smb_2.c:4746-4756, 8179-8184).
- **QUERY_INFO classes:** FileAllInformation, FileInternalInformation, FileStreamInformation, FileFsAttributeInformation, FileFsSizeInformation (class 3, not FullSize), and security (smb_smb_2.c:8448-8490).
- **SET_INFO classes:** Basic, Disposition, EndOfFile, Rename, Allocation, and security.
- **FSCTLs** in the client:
  - VALIDATE_NEGOTIATE_INFO: 3.0/3.0.2 only. STATUS_NOT_SUPPORTED signed is accepted.
  - QUERY_NETWORK_INTERFACE_INFO: only if MULTI_CHANNEL is advertised.
  - SRV_REQUEST_RESUME_KEY, SRV_COPYCHUNK, SRV_ENUMERATE_SNAPSHOTS, GET/SET_REPARSE_POINT, DFS_GET_REFERRALS, PIPE_TRANSCEIVE (IPC$).
  - TM needs none of them.
- **CHANGE_NOTIFY** is sent for watched directories and for the AAPL svrmsg. STATUS_NOT_SUPPORTED makes the client log "polling" and stop (smbfs_notify_change.c:438-441).

## 7. Bonjour

- [TMS] says servers must register `_smb._tcp` (port 445) and `_adisk._tcp` with the same service name. The TXT record is `dk0=adVN=<share>,adVF=0x82` (0x02 means SMB, 0x80 means TM).
- Samba also adds `sys=adVF=0x100` and `_device-info._tcp model=<fruit:model>` (avahi_register.c:142-218).
- Bonjour is only needed for TM's destination picker. `tmutil setdestination smb://…` works without it, as the repo's README and e2e flow show.

## Minimum feature list for a new server (TM, including reconnect)

1. SMB1 NEGOTIATE handling that answers 0x02FF, then SMB2 NEGOTIATE that selects 3.1.1 with the preauth, signing (CMAC or GMAC) and encryption contexts. **High**
2. SPNEGO negTokenInit listing NTLMSSP, NTLMv2 auth, the 3.x KDF, SIGNING_REQUIRED, and a signed final SESSION_SETUP response; accept PreviousSessionId. **High**
3. TREE_CONNECT for one disk share plus IPC$ (or a clean refusal for IPC$). **High**
4. CREATE with the AAPL reply (volume caps FULL_SYNC, server caps 0 or UNIX_BASED) on every connection; MxAc; share modes enforced. **High**
5. FLUSH that is durable (S3-committed) for both Reserved1 values. **High**
6. Correct LARGE_MTU multi-credit READ/WRITE up to 1 MiB, credits granted toward the requested 256, never a zero balance; async STATUS_PENDING for slow operations. **High**
7. Compound requests, related and unrelated. **High**
8. QUERY_DIRECTORY class 37; QUERY_INFO and SET_INFO classes as listed in section 6; FsAttribute with FILE_NAMED_STREAMS; FsSize. **High**
9. Named streams (xattrs, AFP_AfpInfo, AFP_Resource), with the 0-byte-stream rule. **Medium-high**
10. LOCK with FAIL_IMMEDIATELY (no blocking queue); ECHO. **High**
11. For reconnect, all of the following. **High** that it is needed for reconnect; **medium** on the exact timeout.
    - Advertise LEASING.
    - File leases V2 (RWH, at least H) with lease-break support, which is needed once leases are granted.
    - DH2Q honouring Timeout (≤300 s; recommend a 120-180 s default) and CreateGuid replay.
    - Keep opens, share modes and byte-range locks across a disconnect.
    - DH2C reconnect returning the RqLs.
    - Finish the whole reconnect within the 30 s TM dead timer.

## Can omit (with confidence)

- **High:** SMB 2.0.2/2.1; multichannel (do not advertise it); compression; RDMA; DFS (stop advertising the DFS cap); Kerberos; encryption (allowed but not needed); VALIDATE_NEGOTIATE (only for 3.0.x); server-side copy (COPYCHUNK, OSX_COPYFILE); snapshots/timewarp; reparse points; AAPL RESOLVE_ID; HIFI; NFS_ACE; FileFsFullSize; quotas.
- **Medium-high:** directory leases (do not advertise); CHANGE_NOTIFY (return NOT_SUPPORTED); READ_DIR_ATTR enrichment; oplocks other than "none" (macOS uses leases); persistent handles and CA (do not advertise PERSISTENT_HANDLES, which the repo currently does).
- **Medium:** Bonjour `_adisk` (needed only for GUI discovery); ACL fidelity (TM mounts turn ACLs off, smbfs_vfsops.c:411).
- **Low:** dropping named streams for AppleDouble.
