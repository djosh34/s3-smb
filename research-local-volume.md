# A local volume instead of SMB for Time Machine

Question: [#642](https://github.com/djosh34/s3-smb/issues/642), part of map [#633](https://github.com/djosh34/s3-smb/issues/633). Status: done, 2026-10-12. Read-only research. No code changed. Checked against main at 1518da3.

## Summary

- **Time Machine takes two kinds of destination.** A local disk formatted APFS, HFS+ or Xsan, or a network share over SMB. AFP also works up to macOS 26, but Apple says macOS 27 does not support it. NFS, WebDAV, FUSE and FSKit are not on Apple's list. So a FUSE, FSKit or NFS volume cannot be the destination itself. It can only hold a disk image that macOS attaches, with APFS inside.
- **A volume on an attached image as a local destination is not documented by Apple.** Forum posts say `tmutil setdestination /Volumes/<image volume>` worked up to macOS 13 or 14. One blog says that on macOS 26 a hand-made image passes `setdestination` but fails later, during thinning. That is only a weak hint, but it hits the core of the idea. Only a Mac run can settle it.
- **The most credible local route is a loopback NFSv3 server inside s3-smb.** macOS mounts it, attaches a sparsebundle from it, and Time Machine backs up to the APFS volume inside. It needs no kernel extension, no entitlement and no third-party install. macOS's NFS client is maintained. In its default hard mode it waits as long as the server takes. A server restart is invisible to it if file handles stay stable. NFSv3 has no sessions, leases, durable handles, signing or NTLM.
- **FUSE routes are weaker.** The macFUSE kernel backend needs Reduced Security on Apple silicon and a paid licence to bundle. It ejects the volume when the daemon is late (60 s by default, 600 s at most in the last open source). macFUSE's FSKit backend, FUSE-T and FSKit itself are young. FSKit needs a Swift app extension.
- **What a local route removes.** All SMB code: NTLM, signing, credits, leases, durable handles, the one-client rule, interim replies and the 2-minute client limit.
- **What it adds.** The image is attached all the time instead of only during a backup. So any s3-smb exit, S3 stall or sleep problem now hits a mounted APFS volume. Today it only hits a backup in progress. s3-smb would also own creating, attaching and detaching the image, which backupd does today.
- **What it keeps.** The bucket lock and the sleep problem, the S3 request limit, the copy rules and the folder lock. They come from S3 and a possible second server, not from SMB. No route changes them.
- **Verdict.** SMB is the only route Apple documents and the one where macOS manages the image. A loopback NFS route is credible enough to try, but not to commit to. It rests on one undocumented behaviour (Time Machine taking a volume on an attached image), and there is a weak hint that macOS 26 breaks it. Prove the Mac points in "Open points" before choosing it.

## Labels and sources

Each claim has one label:

- **D** documented, with a link.
- **O** observed in our own Mac CI runs, from the earlier reports.
- **S** read in source, with a link to the file and line.
- **I** inferred. The reasoning is given.
- **W** weak hint: a forum post, blog or third-party guide. Not proof.
- **M** can only be settled on a Mac.

Source trees read: Apple [NFS-343.100.5](https://github.com/apple-oss-distributions/NFS/tree/NFS-343.100.5) (April 2026), [xnu-12377.121.6](https://github.com/apple-oss-distributions/xnu/tree/xnu-12377.121.6), [SMBClient-538.121.1](https://github.com/apple-oss-distributions/SMBClient/tree/SMBClient-538.121.1), macFUSE's last open source kernel code ([osxfuse/kext support/osxfuse-3](https://github.com/osxfuse/kext/tree/support/osxfuse-3), 3.8.3, macOS 10.5 to 10.14), [macfuse/macfuse](https://github.com/macfuse/macfuse) and its wiki, [macos-fuse-t/fuse-t](https://github.com/macos-fuse-t/fuse-t) and its wiki, [willscott/go-nfs](https://github.com/willscott/go-nfs), [winfsp/cgofuse](https://github.com/winfsp/cgofuse), and [rclone nfsmount](https://github.com/rclone/rclone/blob/master/cmd/nfsmount/nfsmount.go). Apple's DiskImages, backupd, APFS and the current macFUSE kernel extension are closed source.

Earlier reports: [Time Machine on SMB](https://github.com/djosh34/s3-smb/blob/research/tm-smb-ops/research-tm-smb-ops.md) (#510) and [Mac trace](https://github.com/djosh34/s3-smb/blob/research/tm-mac-trace/research-tm-mac-trace.md) (#528).

## 1. Time Machine destinations

### Local destinations

- Time Machine backs up to "an external storage device connected directly to your Mac (such as a USB or Thunderbolt drive)", or to one partition of such a disk. **D** [Apple, Types of disks you can use with Time Machine](https://support.apple.com/guide/mac-help/mh15139/mac)
- Formats: "APFS or APFS Encrypted disks are the preferred format." "Time Machine still supports backups on Mac OS Extended format (Journaled), Mac OS Extended (Case-sensitive, Journaled), and Xsan formatted disks." Windows formats are not usable. **D** same page.
- "The entire APFS volume is reserved for Time Machine backups." A new disk that is not APFS is offered for erase. **D** same page.
- The page text is the same in the macOS 13, 14, 15, 26 and 27 versions of the guide. **D** same page, version selector. Whether Apple really serves a separate text per version is not visible from outside. **I**
- macOS 28 will drop encrypted HFS+ volumes. Unencrypted HFS+ stays. Encrypted HFS+ Time Machine disks cannot be converted. **D** [Apple 125615](https://support.apple.com/en-us/125615)
- No other file system is listed. So a FUSE, FSKit, NFS or WebDAV volume is not an accepted local destination. **D** by omission on the page above. **I** that backupd checks the volume's file system type; the check itself is not documented.

### Network destinations

- Time Machine backs up to "a third-party NAS device that supports Time Machine over the SMB network protocol", to another Mac sharing a folder, or to an AirPort Time Capsule. **D** [Apple 102423, Backup disks you can use with Time Machine](https://support.apple.com/en-us/102423) (updated 2026-09-14).
- AFP: "not recommended and isn't supported in macOS 27 or later." AirPort Time Capsule uses AFP, so it is affected too. **D** same page.
- So the network protocols are SMB on macOS 13 to 27, and AFP on 13 to 26 only. **D**
- NFS and WebDAV are not listed. **D** by omission.
- For SMB, Apple's spec requires durable v2 handles, v2 leases, durable reconnect and F_FULLFSYNC support. **D** [Time Machine SMB spec](https://developer.apple.com/library/archive/releasenotes/NetworkingInternetWeb/Time_Machine_SMB_Spec/)
- Over a network, backupd creates a sparsebundle, attaches it, formats it APFS, and detaches it after each backup. **O** in our Mac CI ([#510 report](https://github.com/djosh34/s3-smb/blob/research/tm-smb-ops/research-tm-smb-ops.md), section 1).

### Disk images as a local destination

- Apple does not document a volume on an attached disk image (sparsebundle, sparse image or UDIF) as a local destination, for or against. **D** by omission on both pages above.
- backupd clearly handles APFS on an attached sparsebundle, because that is how every network backup works. **O** [#510 report](https://github.com/djosh34/s3-smb/blob/research/tm-smb-ops/research-tm-smb-ops.md). Whether its local-destination checks accept a user-attached image is a separate question. **I**
- A Ventura (macOS 13) user attached a sparsebundle from an external drive and ran `sudo tmutil setdestination` on its volume. The volume took the Time Machine icon and backups ran. **W** [Apple Community thread 254895939](https://discussions.apple.com/thread/254895939) (May 2023).
- A university IT guide describes the same steps with the sparsebundle on "NFS or CIFS storage": attach locally, then `tmutil setdestination '/Volumes/<name>'`. No macOS version is given. **W** [University of Michigan LSA KB 1840](https://teamdynamix.umich.edu/TDClient/47/LSAPortal/KB/ArticleDet?ID=1840)
- A blog says this "worked reliably on Monterey, Ventura, and Sonoma". It also says that on macOS 26, "a manually created disk image passes the initial `setdestination` check but fails during thinning with `TMStructureErrorDomain Code=7` errors". It does not say how the image was made. **W** [ilia.ae, Time Machine Samba NAS: macOS Tahoe](https://ilia.ae/en/blog/digital/macos-time-machine-samba-nas/)
- This is the single most important unknown in this report. **M**

### What `tmutil setdestination` checks

- The current man page: "Configure a local HFS+ or APFS volume, AFP share, or SMB share as a backup destination. Requires root and Full Disk Access privileges." Local volumes are given by mount point, network ones as `protocol://user[:pass]@host/share`. **D** [tmutil(8), mirror at DssW](https://www.dssw.co.uk/reference/tmutil/) (not hosted by Apple; check `man tmutil` on the target Mac). The 2013 page said "local HFS+ volume or AFP share" only. **D** [tmutil(8), 2013 mirror](https://www.unix.com/man-page/osx/8/tmutil/)
- For an SMB URL, tmutil opens its own SMB session to check the share. Our first trace run failed there with "Failed to open session. (error 80)" because of the one-client rule. **O** [#528 report](https://github.com/djosh34/s3-smb/blob/research/tm-mac-trace/research-tm-mac-trace.md), "Run 1 failure".
- For a local path, what it checks beyond "HFS+ or APFS" is not documented: device kind, disk image, internal or external, APFS Backup role. **M**

## 2. Ways to present a local volume without SMB

Every route below gives macOS a file system that is not APFS or HFS+. So each must carry a sparsebundle (or another image format) that macOS attaches, with APFS inside, and Time Machine pointed at that APFS volume. The one exception is a true block device (2d). **I** from section 1.

### 2a. FUSE

**macFUSE, kernel backend**

- Needs a kernel extension. On Apple silicon the user must boot into Recovery, choose "Reduced Security" and turn on "Allow user management of kernel extensions from identified developers", then approve the extension and restart. **D** [macFUSE wiki, Getting Started](https://github.com/macfuse/macfuse/wiki/Getting-Started)
- Apple: "Kexts are no longer recommended for macOS." Enabling them needs the Reduced Security downgrade on Apple silicon. **D** [Apple Platform Security, Securely extending the kernel](https://support.apple.com/guide/security/securely-extending-the-kernel-sec8e454101b/web)
- Supports macOS 12 to 27. Latest is 5.4.0, 2026-09-07. **D** [README](https://github.com/macfuse/macfuse), [releases](https://github.com/macfuse/macfuse/releases)
- The kernel extension is closed source. Bundling with commercial software needs a paid licence. **D** [Open Source Status](https://github.com/macfuse/macfuse/wiki/Open-Source-Status)
- Mature. The developer calls it stable "with a proven track record". **D** [FUSE Backends](https://github.com/macfuse/macfuse/wiki/FUSE-Backends)

**macFUSE, FSKit backend**

- No kernel extension and no Recovery change. Chosen with `-o backend=fskit`. **D** [Getting Started](https://github.com/macfuse/macfuse/wiki/Getting-Started), [FUSE Backends](https://github.com/macfuse/macfuse/wiki/FUSE-Backends)
- "Local" volumes need macOS 15.4 or later. "Non-local" volumes (distributed file systems) need macOS 26. **D** [macFUSE 5.1.0 notes](https://github.com/macfuse/macfuse/releases/tag/macfuse-5.1.0)
- Limits: mount points only under `/Volumes`; files always opened read/write; "most mount options previously handled by the kernel are not implemented, yet"; I/O "not on par" with the kernel backend. **D** [FUSE Backends](https://github.com/macfuse/macfuse/wiki/FUSE-Backends)
- Young. 5.4.0 (September 2026) fixed "a bug that could cause small write requests to deliver zero-filled data to the file system server". **D** [macFUSE 5.4.0 notes](https://github.com/macfuse/macfuse/releases/tag/macfuse-5.4.0). A bug of that kind under a backup image is silent data loss. **I**

**FUSE-T**

- No kernel extension. It runs a local NFSv4 server (or an SMB or FSKit backend) and mounts it with macOS's own client. **D** [README](https://github.com/macos-fuse-t/fuse-t)
- The server is a closed binary. Free for non-commercial use; commercial use or bundling needs a licence. **D** [License.txt](https://github.com/macos-fuse-t/fuse-t/blob/main/License.txt)
- "Currently there's no authentication implemented" on its NFS listener. FALLOCATE, IOCTL and BMAP are unsupported. **D** [FUSE-T wiki](https://github.com/macos-fuse-t/fuse-t/wiki)
- No documented mount options for hard or soft mounts or timeouts. **D** by omission, same page.
- For s3-smb it adds two layers (cgo libfuse, then FUSE-T's NFS server) over plain NFS. A Go NFS server in s3-smb talks to the same macOS NFS client directly. **I**

**Go bindings**

- cgofuse supports macOS through macFUSE (and FUSE-T). It needs cgo. **D** [cgofuse README](https://github.com/winfsp/cgofuse)

### 2b. Apple FSKit directly

- A user-space file system framework. Introduced in macOS 15.4. **D** [FSKit](https://developer.apple.com/documentation/fskit)
- It is shipped as an app extension with the `com.apple.developer.fskit.fsmodule` entitlement. **D** [entitlement](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.developer.fskit.fsmodule). The user must turn the extension on in System Settings, under Login Items & Extensions, File System Extensions. **D** [Building a passthrough file system](https://developer.apple.com/documentation/fskit/building-a-passthrough-file-system)
- "The current version of FSKit supports only `FSUnaryFileSystem`": one resource, one volume. **D** [FSKit](https://developer.apple.com/documentation/fskit)
- Resource kinds:
  - `FSBlockDeviceResource`, a disk partition (IOMedia). macOS 15.4. **D** [doc](https://developer.apple.com/documentation/fskit/fsblockdeviceresource)
  - `FSPathURLResource`, a path. macOS 26. **D** [doc](https://developer.apple.com/documentation/fskit/fspathurlresource)
  - `FSGenericURLResource`, any URL, for example "some sort of network address for a remote file system". macOS 26. **D** [doc](https://developer.apple.com/documentation/fskit/fsgenericurlresource)
- So a network-backed file system needs macOS 26. On 15.4 to 15.x only block-device file systems fit. **D** from the dates above.
- The API changed again in macOS 27. The `Operations` protocols are deprecated in 27.0 and replaced by `Handler` protocols. **D** [FSVolume](https://developer.apple.com/documentation/fskit/fsvolume), [FSVolume.Handler](https://developer.apple.com/documentation/fskit/fsvolume/handler)
- Sync: the volume has one `synchronize(flags:)` call. No per-file fsync appears in `Handler`, `ReadWriteHandler` or `OpenCloseHandler`. **D** [Handler](https://developer.apple.com/documentation/fskit/fsvolume/handler), [ReadWriteHandler](https://developer.apple.com/documentation/fskit/fsvolume/readwritehandler). What F_FULLFSYNC on one file becomes is not documented. **M**
- Request timeouts in FSKit are not documented. **D** by omission. **M**
- A Go core: the API is Swift and Objective-C, so the extension itself must be Swift or Objective-C. It could talk to a Go process over XPC or a socket, which is how macFUSE's FSKit backend reaches its file system process. **I** from [macFUSE 5.2.0](https://github.com/macfuse/macfuse/releases/tag/macfuse-5.2.0) and [5.3.0](https://github.com/macfuse/macfuse/releases/tag/macfuse-5.3.0) notes (mount over XPC, a message channel to the server). Linking Go as a C library into the extension may also work. Not checked. **I**
- Shipping: a Developer ID signed and notarized app with the extension. Whether this entitlement needs Apple's approval for Developer ID distribution is not stated on the entitlement page. **D** by omission. **M** (or ask Apple).
- Not mature for this use. **I** from the API change in 27 and the macFUSE FSKit fixes above.

### 2c. A loopback NFS server

- macOS has an NFS client in a kernel extension Apple still maintains: [NFS-343.100.5](https://github.com/apple-oss-distributions/NFS/tree/NFS-343.100.5), dated April 2026. **S**
- Nothing to install. `mount_nfs` with `port=` and `mountport=` mounts a server on any port, so no portmapper is needed. **D** [go-nfs README](https://github.com/willscott/go-nfs), **S** [rclone nfsmount.go](https://github.com/rclone/rclone/blob/master/cmd/nfsmount/nfsmount.go)
- Prior art in Go: rclone's `nfsmount` starts a Go NFS server and mounts it on macOS. Experimental, since v1.65. It uses go-nfs. **D** [rclone nfsmount docs](https://rclone.org/commands/rclone_nfsmount/), **S** [rclone go.mod](https://github.com/rclone/rclone/blob/master/go.mod)
- go-nfs is NFSv3 (plus some NFSv4), Apache 2.0, "Minimally tested". Its COMMIT is a no-op because it writes through on every WRITE. **D** [README](https://github.com/willscott/go-nfs), **S** [nfs_oncommit.go](https://github.com/willscott/go-nfs/blob/master/nfs_oncommit.go). Its NFSv3, MOUNT and RPC code is about 4,100 lines without tests. **S** (counted). That is a size hint for an s3-smb NFS front end against 12,487 lines of SMB. **I**
- The engine's storage interface (Lookup, Open, ReadAt, WriteAt, Flush, Truncate, GetAttr, SetAttr, ReadDir, Remove, Rename, StatFS) maps onto NFSv3 procedures. NFSv3 has no open or close, so handles would be opened by the server on demand. **S** [`internal/smb/storage.go:147`](https://github.com/djosh34/s3-smb/blob/1518da3/internal/smb/storage.go#L147), **I** for the mapping.
- Access control: NFSv3 normally uses AUTH_SYS, which trusts the uid the client claims. Any local process can reach a loopback port. **I**. The `resvport` option makes the client use a port below 1024, which "root permission is required to mount" with. **D** [mount_nfs(8)](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L183). The server can refuse other source ports, so only root can talk to it. **I**
- Privacy: FUSE-T users need "Network Volumes" access for their terminal, because FUSE-T uses NFS. **D** [FUSE-T README](https://github.com/macos-fuse-t/fuse-t). Whether backupd, DiskImages or a launchd job hit this prompt is not known. **M**
- Does DiskImages attach a sparsebundle that sits on NFS? Our SMB runs show it does on smbfs ("remote mount, barriers not supported"). **O** [#510 report](https://github.com/djosh34/s3-smb/blob/research/tm-smb-ops/research-tm-smb-ops.md). The university guide above says NFS works too. **W**. **M**
- Maturity: the macOS client is mature and is Apple's own. A Go NFS server for this is new code. **I**

### 2d. A user-space block device

- No NBD on macOS. iSCSI needs a third-party initiator. The open source one is a kernel extension and its port off kexts is stalled because DriverKit cannot open sockets. **D** [iscsi-osx/iSCSIInitiator README](https://github.com/iscsi-osx/iSCSIInitiator) (update of 2023-02-28).
- BlockStorageDeviceDriverKit (macOS 12) lets a user-space driver extension present a block device by subclassing `IOUserBlockStorageDevice`. It has read, write, unmap and `DoAsyncSynchronize` calls. **D** [BlockStorageDeviceDriverKit](https://developer.apple.com/documentation/blockstoragedevicedriverkit), [IOUserBlockStorageDevice](https://developer.apple.com/documentation/blockstoragedevicedriverkit/iouserblockstoragedevice)
- Its entitlement "You must request … from Apple". Apple's request process asks for the hardware transport and "your company's hardware vendor ID". **D** [entitlement](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.developer.driverkit.family.block-storage-device), [Requesting Entitlements for DriverKit Development](https://developer.apple.com/documentation/driverkit/requesting-entitlements-for-driverkit-development). Whether Apple would grant it for a virtual disk with no hardware is not known. **I**: unlikely to be quick.
- This is the only route with APFS straight on a "disk" and no image. It would remove DiskImages from the path. It needs C++ in a driver extension, a system extension install approved by the user, and an Apple grant. **I**. Not credible for this project now. **I**
- DiskImages: Apple documents image formats that are files (UDIF, sparse image, sparsebundle, and ASIF on macOS 26). **D** [Disk Utility, types of disk images](https://support.apple.com/guide/disk-utility/types-of-disk-images-you-can-create-dskutl11888/mac). No documented way to back an image by a process. **D** by omission. DiskImages is closed source.

### 2e. Anything else

- **Attach only around a backup.** Turn off automatic backups, and let s3-smb (or a launchd job) attach the image, run `tmutil startbackup --block`, then detach. This copies what backupd does for network destinations, and keeps the image detached most of the time. **I**. Our Mac harness already drives backups with `tmutil startbackup --block`. **O** [#528 report](https://github.com/djosh34/s3-smb/blob/research/tm-mac-trace/research-tm-mac-trace.md). Whether Time Machine tolerates a local destination that is usually missing is **M**.
- **AFP.** Gone in macOS 27. **D** [Apple 102423](https://support.apple.com/en-us/102423)
- **WebDAV.** Not a Time Machine protocol. **D** by omission. Not studied further.
- **Keep SMB, cut it to the loopback subset.** Not a local volume, but it is the baseline every route above is weighed against. It is the subject of [#634](https://github.com/djosh34/s3-smb/issues/634), [#637](https://github.com/djosh34/s3-smb/issues/637) and [#638](https://github.com/djosh34/s3-smb/issues/638).

### Route table

| Route | Install | Reduced Security | Entitlement or approval | macOS | Maturity |
|---|---|---|---|---|---|
| SMB (today) | none | no | none | all | Apple's documented path |
| Loopback NFS + image | none | no | none | all | client mature; server new |
| macFUSE kernel + image | macFUSE pkg, kext approval, restart | yes, on Apple silicon | paid licence to bundle | 12 to 27 | mature |
| macFUSE FSKit + image | macFUSE pkg, enable extension | no | paid licence to bundle | 15.4+ local, 26+ non-local | young |
| FUSE-T + image | FUSE-T pkg | no | commercial licence | not stated | small project, closed server |
| Own FSKit module + image | signed, notarized app; enable extension | no | FSKit entitlement | 26+ for path or URL | young; API changed in 27 |
| DriverKit block device | system extension | no | Apple grant needed | 12+ | none known |

## 3. Failure cases for the credible routes

The three routes judged here: loopback NFS, macFUSE kernel backend, and today's SMB as the baseline. FSKit is left out: its timeouts and sync are not documented.

### How long the client waits

**SMB today**

- The client fails a request 2 minutes after the server last answered it (`SMB_SEND_WAIT_TIMO 60 * 2`). **S** [smb_conn.h:759](https://github.com/apple-oss-distributions/SMBClient/blob/SMBClient-538.121.1/kernel/netsmb/smb_conn.h#L759). That is why s3-smb sends interim replies every 30 s. **D** [#633](https://github.com/djosh34/s3-smb/issues/633)

**NFS, hard mount (the default)**

- "If the server becomes unresponsive while an NFS file system is mounted, any new or outstanding file operations on that file system will hang uninterruptibly until the server comes back (or that NFS file system is forcibly unmounted)." **D** [mount_nfs(8)](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L101). The source agrees: "NFS mounts default to 'rw,hard'". **S** [nfs_socket.c:6501](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/kext/nfs_socket.c#L6501)
- Over TCP the client does not resend a slow request: "for TCP, only resend if explicitly requested". **S** [nfs_socket.c:5851](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/kext/nfs_socket.c#L5851). After 3 s it sends a zero-length probe to check the socket is alive. **S** [nfs_socket.c:4055](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/kext/nfs_socket.c#L4055)
- If no reply of any kind arrives for about 30 s, the mount is marked "not responding" (`NFS_TPRINTF_DELAY 30`, initial 5). **S** [nfs_socket.c:5678](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/kext/nfs_socket.c#L5678), [xnu nfs.h:135](https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/nfs/nfs.h#L135). The request still waits. That state can put up a dialog listing unresponsive file systems. **D** [mount_nfs(8), mutejukebox](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L458)
- The server can instead answer NFS3ERR_JUKEBOX ("try later"). The client waits 4 s, doubling to 30 s, and retries forever on a hard mount. **S** [nfs_socket.c:4873](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/kext/nfs_socket.c#L4873), [xnu nfs.h:102](https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/nfs/nfs.h#L102). **D** [RFC 1813](https://www.rfc-editor.org/rfc/rfc1813) (NFS3ERR_JUKEBOX). The `mutejukebox` option keeps this from counting as "not responding". **D** [mount_nfs(8)](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L458)
- So under NFS there is no 2-minute limit, and nothing like interim replies is needed. The server decides how long a request may take. **I** from the lines above.
- Optional limits: `soft` fails calls after `retrans` timeouts; `deadtimeout=N` force-unmounts a mount that stays unresponsive N seconds. Neither is on by default. **D** [mount_nfs(8)](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L448)
- "Squishy" auto-unmount applies only to automounted mounts on machines marked mobile. **S** [nfs_socket.c:6467](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/kext/nfs_socket.c#L6467). A mount made by s3-smb is not automounted. **I**

**macFUSE kernel backend**

- `daemon_timeout`, default 60 s: "In case there is a timeout, the volume will be automatically ejected (data in any open files may be lost)." **D** [Mount Options](https://github.com/macfuse/macfuse/wiki/Mount-Options)
- In the last open source kernel code, the limit is 0 to 600 s. 0 means wait forever. **S** [fuse_param.h:161-163](https://github.com/osxfuse/osxfuse/blob/archive/osxfuse-3/common/fuse_param.h#L161-L163), [fuse_vfsops.c:478-484](https://github.com/osxfuse/kext/blob/support/osxfuse-3/osxfuse/fuse_vfsops.c#L478-L484)
- On timeout the kernel first sends FUSE_INTERRUPT and waits again. If there is still no answer it marks the file system dead (ENOTCONN) and signals VQ_DEAD. **S** [fuse_ipc.c:204-300](https://github.com/osxfuse/kext/blob/support/osxfuse-3/osxfuse/fuse_ipc.c#L204-L300), [fuse_ipc.c:481-509](https://github.com/osxfuse/kext/blob/support/osxfuse-3/osxfuse/fuse_ipc.c#L481-L509). Whether macFUSE 5 still allows 0 or 600 s is not visible: the kernel code is closed. **M**
- A FUSE daemon has no way to say "still working", like SMB's interim reply or NFS's JUKEBOX. **I** from the code above.

### S3 unreachable for minutes or hours

- **SMB today.** A request fails after s3-smb's 6-minute limit with a device error. backupd fails the backup and tries again later. The lease runs out after about 8 minutes and s3-smb exits. **D** [#633](https://github.com/djosh34/s3-smb/issues/633). The image is only attached during a backup, so between backups nothing is exposed. **O** [#510 report](https://github.com/djosh34/s3-smb/blob/research/tm-smb-ops/research-tm-smb-ops.md)
- **NFS.** s3-smb can hold a request as long as it likes, or answer JUKEBOX. The client waits. **S** above. But the image stays attached, so APFS, DiskImages and any process touching the volume wait too. **I**. Whether DiskImages or APFS has its own timer on backing-file I/O is not documented. **M**
- **macFUSE.** After `daemon_timeout` (plus the interrupt wait) the volume is ejected under the attached image. **D**, **S** above. That is the backing store vanishing under a mounted APFS volume. **I**

### The Mac sleeping

- **SMB today.** The client keeps its session over sleep, and its 2-minute timer does not count sleep. **S**, summarised in [#633](https://github.com/djosh34/s3-smb/issues/633)
- **NFS.** The client kernel extension has no sleep or wake hooks. **S** (no power management calls in [NFS kext](https://github.com/apple-oss-distributions/NFS/tree/NFS-343.100.5/kext) apart from a shutdown check). On loopback both ends sleep together, and the TCP connection should simply continue. **I**. Whether macOS flushes or syncs the attached image before sleep, and whether a stalled sync can delay sleep, is not documented. **M**
- **macFUSE.** Whether `daemon_timeout` counts time asleep is not documented. **M**
- In every route, s3-smb's lease and copy rule count the wall clock. A long sleep still ends the run on wake. **D** [#633](https://github.com/djosh34/s3-smb/issues/633). With a local route that exit now pulls the backing store from under a mounted volume. **I**

### s3-smb crashes or is killed while the volume is attached

- **SMB today.** The client reconnects with durable handles. Time Machine asks a 30 s durable timeout. **D** [#528 report](https://github.com/djosh34/s3-smb/blob/research/tm-mac-trace/research-tm-mac-trace.md), [#633](https://github.com/djosh34/s3-smb/issues/633)
- **NFS.** NFSv3 is stateless. A hard mount blocks while the server is down and goes on when it is back. **D** [mount_nfs(8)](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L101), [RFC 1813](https://www.rfc-editor.org/rfc/rfc1813). To make a restart invisible, s3-smb needs:
  - file handles that stay valid across restarts (for example the engine's inode number plus a fixed volume id)
  - honest WRITE stability. Data acknowledged as UNSTABLE may be lost in a crash; the client keeps it until COMMIT, and resends it when the write verifier changes. **D** [RFC 1813, WRITE and COMMIT](https://www.rfc-editor.org/rfc/rfc1813)

  With launchd's `KeepAlive`, a crash is then a pause, not an error. **I**. This is the one failure case where NFS is clearly better than SMB. **I**
- **macFUSE.** When the daemon's device is closed, as when the process dies, the kernel marks the file system dead and rejects pending answers. **S** [fuse_device.c:262-266](https://github.com/osxfuse/kext/blob/support/osxfuse-3/osxfuse/fuse_device.c#L262-L266), [fuse_ipc.c:481-509](https://github.com/osxfuse/kext/blob/support/osxfuse-3/osxfuse/fuse_ipc.c#L481-L509). A new process cannot take over the old mount. **I** The image's backing store is gone. **I**
- **What APFS does when its image's backing store fails** is not documented. Apple ships network Time Machine with exactly this risk (an image on smbfs), and copes by detaching between backups and by running `fsck_apfs` at attach. **O** [#510 report](https://github.com/djosh34/s3-smb/blob/research/tm-smb-ops/research-tm-smb-ops.md), steps 7 and 12. Whether a local destination gets the same fsck is not known. **M**

### Slow fsync, and whether F_FULLFSYNC reaches s3-smb

- F_FULLFSYNC is "fsync + flush the journal + DKIOCSYNCHRONIZE". The kernel passes it to the file system as an ioctl. F_BARRIERFSYNC turns into F_FULLFSYNC when the file system does not support barriers. **S** [kern_descrip.c:3960-3985](https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/kern/kern_descrip.c#L3960-L3985)
- On an image, DiskImages turns the inner volume's sync into per-band fsyncs and an F_FULLFSYNC on `Info.plist`. Over SMB we saw a data FLUSH on each written band, then data and full FLUSH on `Info.plist`. **O** [#528 report](https://github.com/djosh34/s3-smb/blob/research/tm-mac-trace/research-tm-mac-trace.md), FLUSH section.
- **NFS.** F_FULLFSYNC on an NFS file calls `nfs_flush(..., MNT_WAIT)`: write out dirty data, then COMMIT. **S** [nfs_vnops.c:8109](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/kext/nfs_vnops.c#L8109). So s3-smb sees a COMMIT per file. It cannot tell F_FULLFSYNC from fsync. **I**. That is enough if each band gets its own fsync before the barrier, which is what the SMB trace showed. **I**. A slow COMMIT just waits under a hard mount. **S** above.
- **macFUSE.** In the last open source kernel code, F_FULLFSYNC goes to the same routine as fsync and becomes FUSE_FSYNC. **S** [fuse_vnops.c:1193](https://github.com/osxfuse/kext/blob/support/osxfuse-3/osxfuse/fuse_vnops.c#L1193). A slow FUSE_FSYNC is subject to `daemon_timeout`. **S**
- **FSKit.** Not documented. **M**

### Hung mounts, shutdown and unmount

- **NFS.** At shutdown a hung request on an unresponsive mount returns ESHUTDOWN instead of waiting. **S** [nfs_socket.c:5926](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/kext/nfs_socket.c#L5926). The `intr` option lets a signal end a waiting call. **D** [mount_nfs(8)](https://github.com/apple-oss-distributions/NFS/blob/NFS-343.100.5/mount_nfs/mount_nfs.8#L194)
- **Shutdown order.** launchd stops s3-smb while the image is still attached, so APFS's last flush can go to a server that is gone. s3-smb would have to detach the image in its stop handler before it stops serving. **I**. Whether the image unmounts cleanly at shutdown is **M**.
- **Time Machine's own reaction** to a local destination that fails mid-backup, or to a damaged APFS volume on it, is not documented. **M**

## 4. What each route removes and keeps

### Removed by any local route

All SMB code and promises go (12,487 lines plus 10,368 lines of tests, [#642](https://github.com/djosh34/s3-smb/issues/642)). **I**

- NTLM, signing and encryption, and the SMB password in the System keychain
- credits, sessions and AAPL
- durable handles and their times, leases and the 35 s break timeout
- the one-client rule and the wait after a remount
- interim replies against the 2-minute client limit (NFS waits for the server; FUSE has `daemon_timeout` instead)

### Added by any local route

- **Image lifecycle.** Create the sparsebundle with APFS inside, attach it at boot after the mount, keep it attached, detach it first at shutdown. backupd does all this today. **I**
- **A new failure surface.** The image is attached all the time, not only during a backup. **I**
- **Time Machine setup.** `tmutil setdestination /Volumes/<name>` on an image volume, which Apple does not document. **D** by omission.
- **Front-end code.** An NFSv3 server (about 4,000 lines judging by go-nfs), or cgo with FUSE, or a Swift FSKit extension. **I**

### Per route

| Problem | SMB today | Loopback NFS | macFUSE kernel |
|---|---|---|---|
| 2-minute client limit, interim replies | yes | gone: hard mount waits | gone, replaced by `daemon_timeout`, then eject |
| Durable handles, leases, one-client rule | yes | gone; access limited by reserved port | gone |
| NTLM, signing, keychain password | yes | gone | gone |
| s3-smb restart while in use | durable reconnect | invisible if handles are stable | volume dead, image lost |
| Image attached when idle | no | yes | yes |
| Third-party install, Reduced Security | no | no | yes |
| Bucket lock, sleep exit | kept | kept, and worse: hits a mounted volume | kept, and worse |
| S3 request limit (6 min) | kept; interim replies keep the client waiting | kept as a free choice; the client sets no limit | kept; must end before `daemon_timeout` |
| Copy rules, 30-min stop rule | kept | kept | kept |
| Folder lock, shutdown watchdog | kept | kept, plus detach before stop | kept, plus detach before stop |

### Not changed by any route

- **Bucket lock and Mac sleep.** The lease and the wall clock guard against a second server writing to the same bucket. The front end has no part in that. **D** [#633](https://github.com/djosh34/s3-smb/issues/633), **I**
- **S3 request limit.** Some limit is still needed, so that a stuck S3 call ends. Under NFS the client no longer forces a value, so the limit becomes a free choice between "wait for S3" and "fail the I/O". **I**
- **Copy rules.** Database copies and the 30-minute stop rule guard against losing the data folder. **D** [#633](https://github.com/djosh34/s3-smb/issues/633)
- **Exits.** Under a local route, any exit of s3-smb (lease lost on wake, copy rule, crash) hits an attached volume. So the open decisions in [#635](https://github.com/djosh34/s3-smb/issues/635) and [#640](https://github.com/djosh34/s3-smb/issues/640) matter more, not less. **I**

## 5. Verdict

- **SMB is the only path Apple documents for a destination that is not a physical disk.** It is also the only path where macOS itself creates, attaches, checks and detaches the image. **D**, **O**
- **A local volume is a credible way to drop SMB only through loopback NFS**, with s3-smb serving NFSv3 and a sparsebundle attached from it. It needs nothing installed. The client is Apple's and mature. It has no 2-minute limit. A server restart can be invisible. It removes nearly all of the SMB-specific complexity. **I** from sections 2 and 3.
- **FUSE and FSKit are not credible today.** macFUSE's kernel backend needs Reduced Security and a licence, and ejects on a slow daemon. Its FSKit backend, FUSE-T and FSKit itself are too young, and FSKit's sync and timeouts are undocumented. **I**
- **The NFS route is not ready to commit to.** It rests on one undocumented behaviour: Time Machine accepting a volume on a user-attached image as a local destination. There is a weak hint that macOS 26 breaks this at thinning. It also keeps the image attached all the time, so every s3-smb exit and every long S3 stall hits a mounted APFS volume. **W**, **I**

Recommendation: keep SMB for now. Run the Mac proof below for the NFS route before anyone decides. If the proof passes on macOS 15, 26 and 27, NFS is a real way to remove most of the SMB code.

## Open points: what must be proven on a real Mac

Each is **M**. Run on macOS 15, 26 and 27 where possible, on Apple silicon.

1. **Time Machine accepts it.** Make a sparsebundle with APFS on a local disk. Attach it, run `tmutil setdestination`, do a first backup, then more backups until thinning runs. Repeat with the bundle on a loopback NFS mount. Check that `TMStructureErrorDomain Code=7` does not appear. Try both a hand-made APFS volume and one Time Machine erases itself.
2. **What `setdestination` checks** for a local path: disk image, NFS-backed image, APFS Backup role, encrypted backups.
3. **DiskImages on NFS.** It attaches a sparsebundle from a loopback NFS mount, with `resvport`. What it logs about barriers. Which calls reach the server: WRITE stability, COMMIT per band, COMMIT on `Info.plist`.
4. **s3-smb killed with the image attached and a backup running.** With stable file handles and an honest write verifier, check the backup carries on after launchd restarts it. Then `fsck_apfs` on the volume.
5. **S3 stalled for 10 min, 1 h and 6 h** under a hard mount, during a backup and while idle. Does DiskImages, APFS or backupd give up on its own, and when? Does the "not responding" dialog appear, with and without `mutejukebox`? Test JUKEBOX replies against holding the request.
6. **Sleep** with a request outstanding: sleep is not delayed, and the backup goes on after wake.
7. **Shutdown and restart** with the image attached: s3-smb detaches first and the volume is clean on the next boot. Also with s3-smb already dead: does shutdown hang?
8. **Privacy prompts.** Does a launchd job that mounts NFS and attaches the image, or backupd itself, trigger the "Network Volumes" privacy prompt?
9. **Attach only around a backup** (section 2e): Time Machine tolerates a local destination that is missing between backups.
10. **Restore.** Migration Assistant and "Restore from Time Machine" can see a backup on an image that is attached by hand, and the recovery path for a new Mac works.

Not checked in this report: WebDAV as an image carrier, File Provider, and whether Apple would grant a DriverKit block-storage entitlement for a virtual disk.
