# Passive capture calibration

This branch's `macos.yml` is manual-only and uses one disposable Intel Mac.
It reuses the registered workflow filename; the existing multi-job acceptance
workflow is unchanged on main. It
transfers 19,000,000,000 bytes over one ordinary loopback TCP connection in about
300 seconds. The receiver continuously drains; both peers half-close normally.
Application byte counts and SHA-256 equality are checked without publishing hashes
or payload. It is a throughput calibration, not SMB, Time Machine, recovery, or a
reproduction of a product defect. No TCP, signing, security, or sysctl changes.

Build the reusable helper with:

```sh
cc -O2 -Wall -Wextra -Werror test/macos/passive_capture.c -lpcap -o passive-capture
```

Interface (all paths private; metadata directory must already exist with mode0700):

```text
passive-capture capture IFACE PORT|smb BUFFER_BYTES RAW META_DIR MAX_BYTES MAX_SECONDS MIN_FREE_BYTES
```

`smb` selects both445 and1445; it does not deduplicate PF observations. Start
before any client connects. `ready.json` is published atomically after activation,
filter installation, nonblocking setup and a successful actual-descriptor
`BIOCGBLEN`. It contains requested/effective buffer bytes, snaplen and datalink.
The request is at most32MiB; snaplen262144. Unknown effective allocation fails.
Published XNU's BIOCSBLEN uses a separate upper cap: a512KiB
`debug.bpf_maxbufsize` reading alone does not establish descriptor allocation.

After all client sockets close, send SIGINT and wait at most10 seconds. The helper
drains until1 second idle (at most5 seconds), flushes/fsyncs/closes its dump, records
final receive/drop counters, and closes libpcap. Forced termination, truncation,
zero packet/receive counts, unknown stats, resource limit, or any reported drop
is not healthy. `capture_health_valid` is only capture-process health;
`completeness_proven` is alwaysfalse. It is not a general stream-coverage verdict.
Samples record capture/file byte throughput, native RSS/CPU and free disk once per
second; batch-dispatch and final-flush timings measure observer costs, not endpoint
stall durations. No live per-packet Python or SMB parsing.

Calibration requires at least64GiB free after tooling, caps raw at24GiB and stops
below32GiB free. Before encryption a fresh check requires room for raw+logs+1GiB
conservative archive/cipher expansion plus8GiB reserve; logs are capped at1GiB
and4096files. No compressibility assumption. There is no workload data file. Normal helper RSS is dominated by one
libpcap buffer plus4MiB stdio; kernel BPF buffers are additional. Wall time is
bounded by400-second Python alarm,420-second capture bound,10-second stop wait,
180-second offline audit, and25-minute job deadline. No deletion/rotation of raw.

The exact reviewed `encrypt_capture.sh` and public recipient certificate are
reused. Only public certificate reaches runner. All stdout/stderr remains private;
only numeric allowlists, validated executable/revision/OS provenance and ciphertext
are uploaded. A success marker is emitted only after the encryption helper exits
successfully. Retention happens before offline analysis. If no raw file exists
(early build/activation failure), that helper cannot retain logs: no all-outcome
retention is claimed. Offline checker logs created later are not in the earlier
envelope; only its validated numeric result is public. Raw/auth data is never a
fallback artifact. Remote encrypted retention is7 days; local downloads and any
expansion require a separate size-based resource admission. No local private key
is needed to run this calibration.

`calibration_audit.c` independently scans the closed pcap in constant memory. It
requires one paired IPv4 flow, SYN handshake, exact in-order sequence coverage,
both FINs and terminal ACKs, untruncated records, and exact application byte totals.
It handles32-bit sequence wrap; retransmission/reordering/other flows fail
conservatively. It is not suitable for general Time Machine reassembly, PF alias
deduplication, SMB semantics, or first-divergence proof. Zero drops alone is never
the calibration pass condition. A passing calibration is not a guarantee of zero
drops under a different real Time Machine packet-size/burst/CPU workload.

Tiny tests (set the checker path after compiling it with the same flags):

```sh
CALIBRATION_AUDIT_BIN=/path/to/calibration-audit \
  python3 -B -m unittest discover -s test/macos -p test_capture_calibration.py -v
```

Actual Darwin ioctl/allocation and sustained throughput remain untested until an
explicitly authorized, exact-revision Mac run. Do not dispatch by copying this
command; review and resource allocation are separate prerequisites.
