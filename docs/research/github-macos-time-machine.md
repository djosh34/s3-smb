# GitHub-hosted macOS Time Machine acceptance

> **Historical research, not current acceptance instructions.** The 2026-09-29
> user-approved replacement in issues #34/#18 and [Mac acceptance](../macos-acceptance.md)
> supersedes the source-inventory, capacity-planning, full-content/metadata
> comparison and single-machine sequence below. Current acceptance uses a normal
> full-Mac backup, stopped-MinIO artifact handoff to a second fresh Mac, and native
> restore/comparison of only deliberately created files and folders. Preserve
> this report as evidence of the earlier investigation, not an operative gate.

Research for [issue 32](https://github.com/djosh34/s3-smb/issues/32), 2026-09-27. Input was the `mac-ci` entry in `/tmp/s3-smb-next-research/issues.json` and the user's latest instructions.

Verified session identity, unchanged from the prior audit:

```text
PI_MODEL=gpt-6-astra
PI_PROVIDER=openai-codex
PI_REASONING_LEVEL=xhigh
```

This was a bounded documentation and source investigation. No Actions jobs, Mac probes, builds, permission changes, application changes or subagents ran. Only this research report was written.

## Result

**Subsequent user decision:** the proposed fixture-only backup was rejected. The required final test backs up the Mac runner's normally eligible contents and tests normal and crash recovery from that full backup. Known test files may supplement verification, not replace the full-machine scope. This note establishes neither that GitHub-hosted Time Machine works nor that it is impossible; image provisioning disabling the service is not evidence of a permanent platform restriction.

Actual Time Machine backup and restore on a GitHub-hosted Mac is a plausible final acceptance design, but the official sources do not establish that it will run unattended on a particular runner image. Do not mark it feasible without the final runtime result.

The runner itself is the Mac VM. No nested Mac is needed. GitHub's inspected image configuration explicitly disables Time Machine and unloads its `backupd` service. The image also provisions some Full Disk Access permissions, but that does not prove that the workflow's `tmutil` process receives them. Both need verification in the final task. [1], [3], [4]

Keep the required order:

1. Develop and run the main application E2E suite locally on the Linux VM, with Docker containing the application and MinIO.
2. GitHub Linux CI invokes exactly that same checked-in suite, service definitions, fixture versions, assertions and fault cases. It must not run a reduced CI variant. A matching ARM64 Ubuntu runner is available if the local VM remains ARM64.[1]
3. Actual hosted-Mac Time Machine acceptance is the **dead-last implementation task**, after all other work succeeds. Its first capability checks belong inside that task, not in an earlier exploratory Actions job. It must test a completed backup and normal restore, then a completed baseline followed by a crash during later Time Machine/S3 writes and a successful restore.

An unsupported runner capability leaves this final requirement unmet. Neither generic SMB tests nor a manual user test can replace it.

## What the primary sources establish

### The hosted Mac is already a VM

GitHub documents macOS hosted runners as fresh VMs where `sudo` asks for no password. Its current standard ARM64 table lists three M1 CPU cores, 7 GB RAM and 14 GB SSD storage. `macos-15` is an available explicit OS label. The label still receives image updates, so record the image version and macOS build from the actual run. GitHub documents no nested virtualization support on ARM64 macOS runners; this does not prevent the host macOS guest from running its own Time Machine service.[1]

GitHub limits a hosted job to six hours. Its image-build tests currently check at least 30 GB free space, while the public runner table states 14 GB storage. The image-build check is not a promise about space available to this job after checkout and builds. Measure actual space and arrange enough backup/restore storage for the full-machine test. A resource limit is a constraint to solve or report, not permission to reduce the backup to a fixture. [2], [5]

Do not attempt to reboot or crash the whole hosted VM as the fault injection. That would stop the job and its MinIO process together, rather than isolate an application crash. The proposed test crashes the application process during actual client and S3 activity. It does not claim to simulate hypervisor power loss or S3-provider storage loss.

### Time Machine is present, but the image deliberately disables it

At `actions/runner-images` revision `ede07f8e48022b2c00dc669c7a9d927c46e32a81`, `configure-system.sh`, lines 29-31, contains:

```text
sudo tmutil disable
sudo launchctl unload -w /System/Library/LaunchDaemons/com.apple.backupd.plist
```

The inspected macOS 15 ARM64 image template invokes this script. This is direct source evidence that the image build uses `tmutil` and disables both automatic backups and the daemon. It is not evidence that a delivered workflow can successfully reverse that state.[3]

The final task must inspect the delivered service state and, if needed, enable/load the existing Apple service using the commands supported by that macOS version. Merely turning on automatic backups with `tmutil` is not proof that an unloaded service is running. Do not copy the entire image-build configuration into the test or disable system security protections.

### Root and Full Disk Access are separate requirements

GitHub's `sudo` without a password supplies Unix administrative privileges. Apple separately requires consent for protected file access, including network volumes and full-storage access. Apple's developer documentation says an application cannot grant itself Full Disk Access through code or an entitlement. [1], [6], [7]

The pinned runner image's TCC provisioning script includes `kTCCServiceSystemPolicyAllFiles` entries for `/bin/bash` and runner startup scripts. It also includes network-volume permission entries. This is encouraging evidence for unattended use, not a runtime guarantee. Permissions depend on the responsible process and launch context; do not assume an entry for a shell or Terminal authorizes every workflow child, `sudo` invocation or helper.[4]

The final task should use a known launch path and verify the actual destination-configuration, backup-inspection and restore operations. Successful `sudo`, a readable TCC database, or the presence of a permission row is not sufficient proof. A permission denial or an unattended consent dialog must produce a bounded failure with diagnostics. This note does not propose editing TCC databases, enrolling the runner in MDM, automating privacy-setting clicks or bypassing SIP.

### Exact `tmutil` behavior still belongs to the selected macOS version

Use Apple's real `/usr/bin/tmutil` to configure and inspect Time Machine, start a backup, identify completed backups and restore their backed-up contents. The planned operations are `setdestination`, `destinationinfo`, `startbackup`, `listbackups`, `latestbackup`, `isexcluded` and `restore`. Confirm their supported options and root/FDA requirements from the installed `man tmutil` and help in the final job before constructing the test commands.

I could not retrieve a current Apple-hosted `tmutil` manual from the old Darwin manual URL. The public Apple support pages document Time Machine behavior but are not a complete current CLI contract. I have therefore not certified exact switches, machine-readable output schemas, or per-verb permissions from third-party manual mirrors. The runner source proves use of `tmutil disable`, not successful operation of every required verb.

Do not treat the backup-start command returning as completion. Wait for the actual backup to finish, identify a newly completed backup on the configured destination, and restore from that specific backup. Completion and restore are separate assertions.

## Required scope: full Time Machine backup of the Mac

Back up the runner's normal Time Machine-eligible contents, not a specially restricted fixture volume or directory. The previous recommendation to bound the backup to a controlled dataset was intended to reduce CI resource use; the user rejected that scope reduction.

Apple states that Time Machine does not back up the macOS system files or applications installed with macOS. Those normal native exclusions are different from test-added exclusions that remove ordinary runner content merely to shorten the job. Record the source volumes and exclusions and verify the completed backup and restored contents. [8], [9], [10]

Keep the growing backup destination and restore output outside the backup source, or exclude only the test infrastructure that would otherwise back up itself. Do not exclude SDKs, build trees or user directories just to turn the full backup into a small fixture. Known deterministic files are useful additional checks, but are not the backup scope or the sole restore evidence.

Storage placement and capacity must accommodate the full backup and restoration. Native MinIO on the Mac is only a candidate, not a requirement to squeeze all copies onto that runner's spare disk. If the approved setup cannot meet the full test, report the actual resource or platform blocker; do not silently reduce the source. No hosted runtime or capacity test has been performed.

## Services for this exceptional Mac test

GitHub requires Linux for workflow job containers, Docker actions and service containers. Do not assume that the Linux Docker setup can be transplanted into a macOS `services:` job, or add a nested VM to obtain Docker.[13]

One candidate is native foreground processes on the hosted Mac, subject to enough storage for the full test:

- Build/run the same `s3-smb` revision for Darwin using its normal compiler/SDK requirements.
- Run a native MinIO server with an isolated job-owned data directory and loopback S3 endpoint.
- Use macOS's actual SMB client and Time Machine against the application's share. Keep S3 and SMB traffic on loopback. Check that the required SMB port is available and that the selected listener can bind it; do not assume a nonstandard port works with Time Machine.

MinIO's own source documents standalone `minio server PATH` operation and includes a Darwin disk implementation. That supports this design at source level, not a claim that the selected version builds or runs on this hosted Mac.[14]

There is a current fixture-maintenance limitation. As inspected, `minio/minio` is archived and its README says the project is no longer maintained. It also says community distribution is source-only and legacy binaries no longer receive updates. Do not promise a maintained Darwin download or run an unpinned `@latest`. Use an immutable MinIO revision corresponding to the Linux fixture, record its provenance, and build the Darwin version from that same source where necessary. This research does not replace MinIO with a different product or reopen the chosen Linux test architecture.[14]

This native deployment is an exception for the final Time Machine test only. The main local-Linux and GitHub-Linux suites remain the same Docker suite.

Apple's SMB specification requires behavior beyond generic file access, including SMB 3 signing, durable handles, leases and the `F_FULLFSYNC` extension. Current Apple support also describes connecting to a compatible SMB server without Bonjour discovery. Explicit destination setup avoids a discovery dependency, but the actual client must still accept and use the application-backed share. [15], [16]

## Smallest final-task test sequence

This is a proposed acceptance sequence, not executed instructions or a tested script. The implementation contract additionally requires loss/recreation of daemon-local state in the normal-recovery case, plus resumed Time Machine writes and another restore after crash recovery. Follow the contract and final task for the complete acceptance requirements.

### 1. Check capabilities within the final job

Record the application revision, MinIO revision, runner image version, macOS build, architecture, toolchain and available disk space. Inspect the installed Time Machine CLI and service state. Establish the required permissions, eligible source set and explicit SMB destination without unattended prompts.

If the runner cannot start the service, authorize the required operations, include the source or use the SMB destination, fail this final gate with the exact error. Do not skip it, set `continue-on-error`, fall back to a generic SMB client or delegate completion to a person. No claim about hosted-runner feasibility survives such a failure without a supported solution.

### 2. Complete the full baseline and perform normal recovery

Start with an empty destination and let Apple's Time Machine complete a full backup of the runner's normally eligible contents over the application's SMB share. Record source coverage, exclusions, destination and completed backup identifiers. Additional known checksum files can help verify the result but do not limit its scope.

After Time Machine finishes, require a successful native application metadata backup that contains that completed Time Machine state. These are two different completion events. An S3 metadata export during an unfinished first Time Machine backup is not the required baseline.

After recovering the daemon without its old local state, reconnect and restore the backed-up contents from the completed remote Time Machine backup into a clean restore location. Use Apple's restore operation, not `cp`, `rsync` or a custom sparsebundle reader. Verify restored data/metadata against that backup, rather than a live source that may have changed; selected checksum files alone are not full restore evidence.

Apple documents local APFS snapshots that can restore files without the network destination. Therefore select the remote backup explicitly, reconnect its SMB/image mounts, and retain evidence that restoration read through the application and S3. A successful restore from a local source snapshot would not pass this test.[8]

### 3. Crash during a later real backup, then restore successfully

Reuse or recreate the verified completed baseline. Confirm that its application metadata recovery point is uploaded before beginning later changes. Make controlled additions, changes and deletions within the normal source and start a later Time Machine backup. These changes make the crash reproducible; they do not replace the full baseline with a fixture-only backup.

Observe and hold a real S3 write generated by that backup, reusing the Linux suite's fault controls where applicable, then abruptly terminate `s3-smb`. Record that Time Machine was still active and that the S3 operation had not completed. A fixed sleep followed by a kill does not establish the required crash window. Keep MinIO and its already committed objects intact.

Leave the application's ordinary metadata-protection behavior in the scenario. Record whether a newer metadata backup captured the later Time Machine activity. A coherent JuiceFS metadata export does not by itself prove that the Time Machine disk image inside it is recoverable. Do not quietly choose an older handpicked export merely to avoid exercising the normal recovery policy.

Stop the interrupted client operation and detach stale destination mounts. Recover the server through its documented S3 recovery path with fresh application metadata/cache, preserving only the simulated remote object store and documented recovery inputs. Drive any required application confirmation through a test-controlled pseudo-terminal, not a human prompt. Do not repair the remote objects or alter Time Machine's backup files to make the test pass.

Reconnect with Apple's client and restore the entire completed baseline into an empty location. Verify its manifest. The interrupted later backup may be absent or incomplete; it must not make the completed baseline unreadable. Record which application recovery point and Time Machine backup actually supplied the restore.

This proves the tested application-crash boundary only. It does not prove recovery from every timing boundary, a MinIO disk failure, a crash of the Mac VM, or an S3-provider outage. If Time Machine refuses the recovered image or the baseline fails comparison, the acceptance test fails even if SMB files can still be listed.

## What only the final runtime test can establish

- Whether the delivered runner's `backupd` service can operate after image provisioning disabled it.
- Whether the actual workflow launch context has the root/FDA/TCC permissions needed for configuration, backup inspection and restore without UI intervention.
- Whether the full normally eligible source can be backed up and restored with the approved storage arrangement and hosted job limits, without reducing it to a test dataset.
- Whether the application, the pinned MinIO deployment and the SMB destination work with Time Machine on the actual runner.
- Whether Time Machine completes the full Mac backup and Apple's restore path returns the backed-up contents from the remote destination.
- Whether fresh application recovery after the observed in-flight-write crash preserves a Time Machine backup that Apple's client can actually open and restore.

No result above is established by Linux Docker tests, a Darwin build, an SMB smoke test, runner-image source inspection or this note. The final task remains mandatory and last.

## Primary sources

GitHub and Apple documentation below was read on 2026-09-27. Mutable documentation describes current published policy; pinned image source describes that source revision, not necessarily the VM assigned to a later job.

[1]: https://docs.github.com/en/actions/reference/runners/github-hosted-runners
[2]: https://docs.github.com/en/actions/reference/limits
[3]: https://github.com/actions/runner-images/blob/ede07f8e48022b2c00dc669c7a9d927c46e32a81/images/macos/scripts/build/configure-system.sh#L29-L31
[4]: https://github.com/actions/runner-images/blob/ede07f8e48022b2c00dc669c7a9d927c46e32a81/images/macos/scripts/build/configure-tccdb-macos.sh#L40-L48
[5]: https://github.com/actions/runner-images/blob/ede07f8e48022b2c00dc669c7a9d927c46e32a81/images/macos/scripts/tests/System.Tests.ps1
[6]: https://support.apple.com/guide/security/controlling-app-access-to-files-secddd1d86a6/web
[7]: https://developer.apple.com/documentation/security/accessing-files-from-the-macos-app-sandbox#Request-full-disk-access-to-use-all-files-on-a-Mac
[8]: https://support.apple.com/guide/mac-help/back-up-files-mh35860/mac
[9]: https://support.apple.com/guide/mac-help/exclude-files-from-a-time-machine-backup-mh15622/mac
[10]: https://support.apple.com/guide/mac-help/restore-items-backed-up-with-time-machine-mh11422/mac
[11]: https://support.apple.com/guide/disk-utility/add-delete-or-erase-apfs-volumes-dskua9e6a110/mac
[12]: https://support.apple.com/guide/deployment/time-machine-payload-settings-dep1cddddk7/web
[13]: https://docs.github.com/en/actions/tutorials/use-containerized-services/use-docker-service-containers
[14]: https://github.com/minio/minio/blob/7aac2a2c5b7c882e68c1ce017d8256be2feea27f/README.md
[15]: https://developer.apple.com/library/archive/releasenotes/NetworkingInternetWeb/Time_Machine_SMB_Spec/
[16]: https://support.apple.com/guide/mac-help/types-of-disks-you-can-use-with-time-machine-mh15139/mac

Additional inspected source:

- [macOS 15 ARM64 image template](https://github.com/actions/runner-images/blob/ede07f8e48022b2c00dc669c7a9d927c46e32a81/images/macos/templates/macOS-15.arm64.anka.pkr.hcl) invokes both TCC provisioning and the final system-configuration script.
- [MinIO Darwin disk implementation](https://github.com/minio/minio/blob/7aac2a2c5b7c882e68c1ce017d8256be2feea27f/internal/disk/directio_darwin.go) provides platform-specific source, not a build or runtime result.
- [MinIO repository metadata](https://api.github.com/repos/minio/minio) reported `archived: true` during this inspection.

Apple's Time Machine SMB specification is an archived protocol reference. Current Apple support establishes the present destination/exclusion behavior cited above. Neither provides a hosted-runner certification or replaces the final client test.
