# Research: S3 guarantees a database-free design can rely on

Ticket: [#511](https://github.com/djosh34/s3-smb/issues/511). Map: [#173](https://github.com/djosh34/s3-smb/issues/173).
Date: 2026-10-05.

Scope: AWS S3 (baseline), Backblaze B2 (S3-compatible API) and Garage.
MinIO, Ceph RGW and Wasabi are covered in the [partial findings comment on #511](https://github.com/djosh34/s3-smb/issues/511) and are not repeated here.

Sources are official docs, quoted and linked. For Garage, the source code on `main-v2` at commit `adaf85c` (2026-10-02, version 2.4.1) fills gaps.
Anything marked **(inferred)** is my reading, not a documented promise.

Design constraints from #173: no database, no GC, no trash, no snapshots. Nothing in the bucket may be orphaned. No local state; a scratch disk is allowed only if it can be wiped at any time. No encryption, no compression. Crash rule: after a crash every file equals its content at its last FLUSH, and unflushed writes may land partially, like on a local disk. A 5-minute S3 outage must only slow a backup down.

## 1. Summary

- **Conditional writes are not available on either target.** Garage says they are "structurally impossible". B2 does not document them, and third-party reports say B2 ignores `If-None-Match` or answers 501. Only AWS has them. A safe single-writer lock built on S3 is therefore not possible on B2 or Garage.
- **Rebuilding a band per FLUSH (layout a) is the wrong shape.** Each FLUSH copies about 250 MiB server-side per dirty band. Holes are stored as zeros. On B2 every rebuilt band also leaves a full hidden old version that is billed until a lifecycle rule deletes it.
- **Fixed chunk objects (layout b) fit the crash rule with plain PutObject.** Every chunk PUT is atomic. A crash during FLUSH leaves a mix of old and new chunks, which is exactly "unflushed writes land partially". There is no multipart upload, so nothing can be orphaned by a crash.
- **Recommended: layout (b) with 4 MiB chunks.** It needs PutObject, ranged GetObject, DeleteObject, ListObjectsV2 and strong read-after-write. Overwritten bytes must be freed by the backend, directly or by a lifecycle rule. B2 and Garage both provide this set, with two caveats: B2 does not document its consistency, and B2 needs the "Keep only the last version" lifecycle rule.

## 2. Per-provider facts

### 2.1 AWS S3 (baseline)

**UploadPartCopy and limits.** [Multipart limits](https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html): "Part size: 5 MiB to 5 GiB. There is no minimum size limit on the last part of your multipart upload." "Maximum number of parts per upload: 10,000." "Maximum object size: 48.8 TiB." [UploadPartCopy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPartCopy.html): "The range value must use the form bytes=first-last ... You can copy a range only if the source object is greater than 5 MB." [CompleteMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CompleteMultipartUpload.html) fails with `EntityTooSmall`: "Each part must be at least 5 MB in size, except the last part."
Copying from the key being overwritten: nothing in the docs forbids it. The copy source is the current committed version, and the new upload is invisible until Complete. **(inferred)** This works.

**Does server-side copy rewrite bytes? Cost and latency.** Not documented. [CopyObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CopyObject.html): "The copy request charge is based on the storage class and Region that you specify for the destination object." The copy is billed as a new object of full size. **(inferred)** Treat it as a full rewrite. No copy latency or throughput figure is published. It must be measured.
Prices, us-east-1 Standard, from the [AWS price list](https://aws.amazon.com/s3/pricing/): storage $0.023 per GB-month; PUT, COPY, POST, LIST $0.005 per 1,000; GET $0.0004 per 1,000; "DELETE and CANCEL requests are free."
Copy responses can hide errors: "A `200 OK` response can contain either a success or an error." CompleteMultipartUpload is similar: "The processing of a CompleteMultipartUpload request could take several minutes to finalize ... Amazon S3 periodically sends white space characters". The client must parse the body.

**Atomicity.** [Consistency model](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Welcome.html#ConsistencyModel): "Updates to a single key are atomic. For example, if you make a PUT request to an existing key from one thread and perform a GET request on the same key from a second thread concurrently, you will get either the old data or the new data, but never partial or corrupt data." "There is no way to make atomic updates across keys." Concurrent writers: "If two PUT requests are simultaneously made to the same key, the request with the latest timestamp wins."

**Read-after-write and LIST.** Same page: "Any read (GET or LIST request) that is initiated following the receipt of a successful PUT response will return the data written by the PUT request." It also lists delete-then-list: "The object does not appear in the listing."

**Conditional writes.** [Conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html): `If-None-Match` and `If-Match` are supported on PutObject, CompleteMultipartUpload and CopyObject. "If multiple conditional writes or copies occur for the same object name, the first write operation to finish succeeds. Amazon S3 then fails subsequent writes with a `412 Precondition Failed` response." A 409 is possible with concurrent deletes. "When using `CompleteMultipartUpload`, the entire multipart upload must be re-initiated." AWS is the only target where a lock object can be safe.

**User metadata.** [Object metadata](https://docs.aws.amazon.com/AmazonS3/latest/userguide/UsingMetadata.html): "Within the `PUT` request header, the user-defined metadata is limited to 2 KB in size." "After you upload the object, you can't modify this user-defined metadata. The only way to modify this metadata is to make a copy of the object and set the metadata." CopyObject works "up to 5 GB in size in a single atomic action" and takes `x-amz-metadata-directive: REPLACE`. Whether that copies the data bytes is not documented.

**Rename.** [RenameObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_RenameObject.html) exists only for directory buckets: "`RenameObject` is only supported for objects stored in the S3 Express One Zone storage class." Not usable for general purpose buckets.

**Abandoned multipart uploads.** [Multipart overview](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html): "Amazon S3 retains all the parts until you either complete or stop the upload. Throughout its lifetime, you are billed for all storage, bandwidth, and requests for this multipart upload and its associated parts." "There are no early delete charges for deleting incomplete multipart uploads regardless of storage class specified." Cleanup: a lifecycle rule with `AbortIncompleteMultipartUpload` and `DaysAfterInitiation` ([doc](https://docs.aws.amazon.com/AmazonS3/latest/userguide/mpu-abort-incomplete-mpu-lifecycle-config.html)), or ListMultipartUploads plus AbortMultipartUpload.

**Overwrites, deletes and minimum durations.** S3 Standard has no minimum storage duration. The [pricing page](https://aws.amazon.com/s3/pricing/): "S3 Standard-IA and S3 One Zone-IA storage are charged for a minimum storage duration of 30 days. Objects that are deleted, overwritten, or transitioned ... before the minimum storage duration will incur the normal storage usage charge plus a pro-rated charge". Glacier Instant and Flexible: 90 days. Deep Archive: 180 days. Only Standard fits frequent overwrites.

**Versioning and Object Lock.** Versioning is off by default. [Versioning](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Versioning.html): "Each version of an object is the entire object; it is not just a diff from the previous version." [Object Lock](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html): "Object Lock works only in buckets that have S3 Versioning enabled." "Retention periods and legal holds don't prevent new versions of the object from being created". With frequent overwrites, every overwrite keeps a full billed copy, and locked versions cannot be deleted until retention ends. Both must stay off.

**LIST.** [ListObjectsV2](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html): "Returns some or all (up to 1,000) of the objects in a bucket with each request", in lexicographical key order, paged by `ContinuationToken`. LIST costs the same as PUT. 70,000 keys need 70 requests, about $0.00035. Request rate: "at least 3,500 PUT/COPY/POST/DELETE or 5,500 GET/HEAD requests per second per partitioned Amazon S3 prefix" ([performance](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html)).

### 2.2 Backblaze B2 (S3-compatible API)

**UploadPartCopy and limits.** [S3 Upload Part Copy](https://www.backblaze.com/apidocs/s3-upload-part-copy) is supported, with part numbers "between 1 and 10,000". The S3 page documents only `x-amz-copy-source`. The native [b2_copy_part](https://www.backblaze.com/apidocs/b2-copy-part) takes "The range of bytes to copy", example `bytes=0-4999999`. **(inferred)** `x-amz-copy-source-range` maps onto it. [Large files](https://www.backblaze.com/docs/cloud-storage-large-files): "Each part can be anywhere from 5 MB to 5 GB". "Each large file must consist of at least two parts, and all of the parts except the last one must be at least 5 MB." B2 defines 5 MB as "5,000,000 bytes" ([S3 Create Multipart Upload](https://www.backblaze.com/apidocs/s3-create-multipart-upload)). b2_copy_part: "The parts uploaded for one file must have contiguous numbers, starting with 1." AWS allows gaps; B2 does not. Copying from the key being overwritten is not addressed. **(inferred)** It works, as on AWS.

**Copy rewrite, cost and latency.** Large files doc: "You are charged for storage for the parts that you uploaded or copied. Usage is counted from the time the part is stored." So a copy is billed as new storage. Whether bytes are physically rewritten is not documented. No latency figure. [Transaction pricing](https://www.backblaze.com/cloud-storage/transaction-pricing): UploadPartCopy and CopyObject are Class C, and Class A, B and C are free for pay-as-you-go ([pricing](https://www.backblaze.com/cloud-storage/pricing)). Storage is $6.95 per TB-month.

**Atomicity.** Not documented for the S3 API. **(inferred)** PutObject and CompleteMultipartUpload are atomic: a new version becomes visible as a whole when the upload finishes.

**Read-after-write and LIST.** Not documented by Backblaze. I found no official page that promises read-after-write or list-after-write. Community posts claim B2 is strongly consistent ([Veeam forum](https://forums.veeam.com/object-storage-f52/do-we-need-to-be-treating-backblaze-s3-compat-repos-as-eventually-consistent-t73262.html)), but no Backblaze statement. This is a risk. The design should get a written answer from Backblaze or test it.

**Conditional writes.** Not documented on [S3 Put Object](https://www.backblaze.com/apidocs/s3-put-object) or [S3 Complete Multipart Upload](https://www.backblaze.com/apidocs/s3-complete-multipart-upload). Third-party reports, not official: B2 accepted the header and ignored it ([hashicorp/terraform#37143](https://github.com/hashicorp/terraform/issues/37143), 2025-05), and B2 "answers 501 to `If-None-Match: *`" ([harochell-tech/ERP#112](https://github.com/harochell-tech/ERP/pull/112), 2026-09). Either way, there is no lock primitive.

**User metadata.** [File information](https://www.backblaze.com/docs/cloud-storage-file-information): "Backblaze B2 limits the combined header size for the file name and all file information to 7,000 bytes." "For files that are encrypted with server-side encryption or files that are in Object Lock-enabled buckets, the limit is reduced to 2,048 bytes". Metadata changes need a copy: [b2_copy_file](https://www.backblaze.com/apidocs/b2-copy-file) has `metadataDirective` COPY or REPLACE. Whether a REPLACE copy rewrites data is not documented.

**Rename.** None. [Files](https://www.backblaze.com/docs/cloud-storage-files): "After a file is uploaded, you cannot change this name."

**Abandoned multipart uploads.** Billed: "Usage is counted from the time the part is stored." Cleanup: lifecycle rule `daysFromStartingToCancelingUnfinishedLargeFiles` ([lifecycle rules](https://www.backblaze.com/docs/cloud-storage-lifecycle-rules)), which must be a positive number of days. ListMultipartUploads and AbortMultipartUpload are supported (listed in transaction pricing).

**Overwrites, deletes and minimum durations.** Pricing: "No minimum storage duration fees". But B2 is always versioned. [S3 API intro](https://www.backblaze.com/apidocs/introduction-to-the-s3-compatible-api): "Buckets in Backblaze B2 are versioned by default. Because buckets are versioned, when a file is deleted by referencing the name, only the most recent version of that file is deleted and older versions of the file continue to exist in the bucket." [Lifecycle rules](https://www.backblaze.com/docs/cloud-storage-lifecycle-rules): "By default, older versions are kept forever". The bucket option "Keep only the last version of the file" means: "The previous version of the file is 'hidden' for one day and then deleted." So every overwrite and every delete keeps the old bytes billed for at least a day. A delete with an explicit version ID removes a version at once. The S3 lifecycle API is not in B2's list of S3 calls. **(inferred)** The rule is set in the web console or with the native `b2_update_bucket`.

**Versioning and Object Lock.** Versioning cannot be turned off (see above). [Object Lock](https://www.backblaze.com/docs/cloud-storage-object-lock): "after you enable Object Lock on a bucket, you cannot disable this setting." With frequent overwrites, locked versions pile up. The bucket must not use Object Lock.

**LIST.** [S3 List Objects V2](https://www.backblaze.com/apidocs/s3-list-objects-v2): "Returns a list of up to 1,000 objects in the bucket, sorted alphabetically by key." Free (Class C).

### 2.3 Garage

**UploadPartCopy and limits.** [S3 compatibility](https://garagehq.deuxfleurs.fr/documentation/reference-manual/s3-compatibility/) marks UploadPartCopy as implemented. Source (`src/api/s3/copy.rs`): exactly one range must be given. Source objects small enough to be inlined in metadata cannot be part-copied. The code enforces no 5 MiB minimum and no 10,000 part maximum; Complete rejects only an empty part list (`multipart.rs`). Part numbers need not be contiguous: "it doesn't need to start at 1 nor to be a continuous sequence". **(inferred from source)** Copying from the key being overwritten works, because the copy reads the current complete version.

**Copy rewrite, cost and latency.** Garage splits objects into blocks (default `block_size` 1 MiB) addressed by hash. For UploadPartCopy the source says: "We want to reuse blocks from the source version as much as possible. However, we still need to get the data from these blocks because we need to know it to calculate the MD5sum of the part". Whole blocks are reused without new storage; partial blocks at range edges are rewritten. Every copied byte is still read. CopyObject reuses blocks unless encryption or a checksum change forces a rewrite. Self-hosted, so there is no request billing. Latency depends on disks and network.

**Atomicity.** Not stated in docs. Source: an object version is `Uploading`, `Complete` or `Aborted` (`src/model/s3/object_table.rs`). **(inferred)** Reads see only complete versions, so Put and Complete are atomic per key. Concurrent writes are resolved by timestamp.

**Read-after-write and LIST.** [Configuration](https://garagehq.deuxfleurs.fr/documentation/reference-manual/configuration/), `consistency_mode`: "`consistent`: The default setting ... The read and write quorum will be determined so that read-after-write consistency is guaranteed." "`degraded`: ... In this mode, Garage does not provide read-after-write consistency anymore." **(inferred)** LIST uses the same quorum tables and is consistent in `consistent` mode.

**Conditional writes.** [Known issues](https://garagehq.deuxfleurs.fr/documentation/reference-manual/known-issues/): "No conditional writes / locking / WORM support (`if-none-match`, ...). This is structurally impossible to implement in Garage due to the lack of a consensus algorithm, which is one of Garage's core design choices which we cannot reconsider." It also says many practical uses cannot be supported, "e.g. using it to implement mutual exclusion between concurrent writers". Source confirms `If-None-Match` is read only for GET and for copy-source preconditions.

**User metadata.** No limit is documented. **(inferred)** Bounded by HTTP header size. Metadata changes need CopyObject with REPLACE, which reuses data blocks.

**Rename.** None.

**Abandoned multipart uploads.** `PutBucketLifecycleConfiguration` is partial: "The only actions supported are AbortIncompleteMultipartUpload and Expiration". The lifecycle worker runs once a day from midnight (`src/model/s3/lifecycle_worker.rs`). ListMultipartUploads and AbortMultipartUpload are implemented. Storage is the operator's own disk.

**Overwrites, deletes and minimum durations.** No billing. No versioning, so an overwrite drops the old version. Garage frees unreferenced blocks internally.

**Versioning and Object Lock.** "Garage does not (yet) support object versioning." Object Lock endpoints are all missing.

**LIST.** ListObjectsV2 is implemented; `max-keys` is clamped to 1 to 1,000 (`src/api/s3/api_server.rs`).

**Durability caveats.** `metadata_fsync` and `data_fsync` are both "disabled (`false`) by default". Known issues list "LMDB metadata corruption ... after a forced shutdown of Garage or in case of power loss." For FLUSH to mean durable on Garage, the operator needs `replication_factor` of at least 2 and should enable the fsync options. This is an operator setting, not something s3-smb can check.

### 2.4 Feature table

| Feature | AWS S3 | Backblaze B2 | Garage |
|---|---|---|---|
| UploadPartCopy with range | Yes. 5 MiB to 5 GiB, 10,000 parts. Range only if source > 5 MB | Yes. 5,000,000 B to 5 GiB, 10,000 parts, contiguous numbers. Range documented on native API only | Yes, one range. No size minimum enforced |
| Server-side copy rewrites bytes | Not documented. Billed as a new object | Not documented. Copied parts billed as storage | Whole blocks reused, edge blocks rewritten. All bytes read |
| Copy cost | $0.005 per 1,000 | Free | Own hardware |
| Atomic PutObject and Complete | Documented | Not documented (inferred yes) | Not documented (inferred yes from source) |
| Read-after-write, LIST-after-write | Documented, strong | Not documented | Documented in `consistent` mode; LIST inferred |
| If-None-Match / If-Match on Put and Complete | Yes | No | No, by design |
| User metadata | 2 KB. Change via copy | 7,000 B (2,048 B with SSE or Object Lock). Change via copy | No documented limit. Copy reuses blocks |
| Rename | Express One Zone only | No | No |
| Abandoned MPU | Billed. Lifecycle abort, min 1 day | Billed. Lifecycle cancel, min 1 day | Lifecycle abort, daily worker |
| Overwrite and delete billing | Standard: none. IA 30 d, Glacier 90 to 180 d | Old versions billed until deleted. "Keep only last version" deletes after 1 day | None |
| Versioning | Optional, off by default | Always on | None |
| Object Lock | Optional, needs versioning | Optional, cannot be disabled | None |
| LIST page and cost | 1,000 per page, $0.005 per 1,000 | 1,000 per page, free | 1,000 per page |

## 3. Workload model

From `research-storage-engine.md` (branch `research/plan-v0.2`):

- Bands are about 268 MB (256 MiB), about 4,000 per TB.
- WRITEs are 1 MiB, 512 KiB or 256 KiB (46/46/8% by bytes) at random offsets inside existing bands. APFS writes new data copy-on-write, mostly in sequential runs. APFS metadata is overwritten in place in hot, low-numbered bands.
- Bands are sparse. Holes read as zeros and should not cost storage.
- FLUSH arrives on each dirty band handle every few seconds to tens of seconds, and at snapshot and backup end. FLUSH is the only durability point.

Four FLUSH scenarios for one 256 MiB band:

- **S1**: one 1 MiB overwrite (hot metadata).
- **S2**: eight 256 KiB overwrites at random offsets.
- **S3**: one 16 MiB sequential run into a hole region.
- **S4**: a new band filled from empty in 16 FLUSHes of 16 MiB each (totals for the whole band).

"GET" below is the read-modify-write read of clean bytes when they are not in memory. A memory cache removes some of them.

## 4. Layouts

### 4.1 Layout (a): one object per file, rebuilt on FLUSH with multipart and part copy

At FLUSH: CreateMultipartUpload, then for each segment of the band either UploadPart (dirty data, padded to at least 5 MiB with clean bytes) or UploadPartCopy (clean range of the old object), then CompleteMultipartUpload.

**Requests and bytes per FLUSH.** Two variants: fixed 8 MiB parts (32 parts), or the fewest variable segments.

| Scenario | Fixed 8 MiB parts: requests | Upload | GET | Server-side copy | Variable segments: requests | Upload | GET | Copy |
|---|---|---|---|---|---|---|---|---|
| S1 | 34 + 1 GET | 8 MiB | 7 MiB | 248 MiB | 5 + 1 GET | 5 MiB | 4 MiB | 251 MiB |
| S2 | 34 + 8 GET | 64 MiB | 62 MiB | 192 MiB | 19 + 8 GET | 40 MiB | 38 MiB | 216 MiB |
| S3 | 34 + up to 2 GET | 16 to 24 MiB | 0 to 8 MiB | 232 to 240 MiB | 5 | 16 MiB | 0 | 240 MiB |
| S4 (total) | 16 rebuilds | 256 MiB | some | about 1,920 MiB | 16 rebuilds | 256 MiB | 0 | about 1,920 MiB |

The holes in S3 are stored as real zeros inside the object. Without a hole map, the server cannot know they are zeros, so it may GET them. S4 is quadratic: each FLUSH copies everything already in the band.

**Crash during FLUSH.** Complete is atomic, so the object is the old version or the new one. The old version is the content at the last FLUSH. This meets the crash rule.

**5 MiB minimum part size.** Every dirty extent must grow to a 5 MiB part, filled with clean bytes fetched by GET. A clean gap under 5 MiB between two dirty extents must also become an upload part. On AWS a range copy needs a source "greater than 5 MB". On B2 part numbers must be contiguous, and a large file needs at least two parts. Use 5 MiB as the floor, since it also covers B2's 5,000,000 bytes.

**Orphans.** A crash leaves an unfinished multipart upload, billed on AWS and B2. Avoiding it needs a startup sweep (ListMultipartUploads plus Abort) or a lifecycle rule. On B2 every rebuild also leaves a full 256 MiB hidden version. With a FLUSH every 10 s on one hot band, that is about 2.2 TB of hidden versions per day, each kept at least a day. That is far too costly. Deleting the old version by ID right after Complete narrows the window, but a crash in between still leaves a hidden version.

**Memory with no local disk.** About one part per dirty extent: 5 to 8 MiB for each scattered write, plus the GET buffer. Parts can be uploaded before FLUSH into an open upload, so memory can stay under a cap. Memory is not the problem with (a). Server-side copy volume and latency are.

**Verdict.** It works on all three backends. It is wrong for this workload: about 250 MiB copied per dirty band per FLUSH, quadratic band growth, stored holes, multipart orphans, and high cost on B2.

### 4.2 Layout (b): fixed-size chunk objects per file

Each file is a sequence of chunk objects of size C, one key per chunk index, for example `<file key>/<index as 8 hex digits>`. The exact key scheme is for a later ticket. A write dirties whole chunks in memory. FLUSH PUTs each dirty chunk with PutObject. Reads are ranged GETs.

**Exact size and holes without a database.**

- A missing chunk is a hole and reads as zeros. An all-zero chunk is deleted rather than stored.
- Trailing zeros of a chunk are trimmed before PUT. A chunk shorter than C is zero-padded on read.
- The file size is the end of its last chunk object: `index × C + length`. That one chunk keeps its full length, including trailing zeros, so the size is exact.
- ListObjectsV2 returns each key with its size, so one LIST over a file's prefix gives its size and hole map. One LIST over the whole bucket gives every file's size. The server holds this in memory and rebuilds it from the bucket on start. No metadata header or head object is needed.
- A file that ends in a hole after a set-EOF must store a zero-filled last chunk of up to C bytes. **(inferred)** This is rare for Time Machine.

**Requests and bytes per FLUSH.** S2 uses the expected number of distinct dirty chunks.

| Scenario | C = 4 MiB | C = 8 MiB | C = 16 MiB | C = 64 MiB |
|---|---|---|---|---|
| S1: requests | 1 PUT + 1 GET | 1 + 1 | 1 + 1 | 1 + 1 |
| S1: up / down | 4 / 3 MiB | 8 / 7 MiB | 16 / 15 MiB | 64 / 63 MiB |
| S2: requests | ~8 PUT + ~8 GET | ~7 + ~7 | ~6.5 + ~6.5 | ~3.6 + ~3.6 |
| S2: up / down | ~30 / ~28 MiB | ~57 / ~55 MiB | ~103 / ~101 MiB | ~230 / ~228 MiB |
| S3: requests (aligned) | 4 PUT | 2 PUT | 1 PUT | 1 PUT |
| S3: up / down | 16 / 0 MiB | 16 / 0 MiB | 16 / 0 MiB | 16 / 0 MiB |
| S4 total: up / down | 256 / 0 MiB | 256 / 0 MiB | 256 / 0 MiB | 640 / up to 384 MiB |
| Objects per TB | ~250,000 | ~125,000 | ~62,000 | ~16,000 |
| Full LIST at start (pages) | ~250 | ~125 | ~62 | ~16 |

There is no server-side copy. Bytes moved scale with the dirty data, not with the band size. With C = 64 MiB a run spread over several FLUSHes rewrites the same chunk again and again (S4).

**Crash during FLUSH.** Each chunk PUT is atomic, so each chunk is either its old or its new content. After a crash during FLUSH N, the file is its FLUSH N-1 content plus some of the writes since. Those writes were never acknowledged as durable, so that is the "land partially" case of the crash rule. A write that crosses a chunk boundary can tear. A local disk can tear a write too.
Two rules keep this true. First, a FLUSH is acknowledged only after all its chunk PUTs succeeded. Second, there is at most one PUT in flight per key, so an older PUT cannot overtake a newer one.

**5 MiB minimum part size.** Not relevant. Layout (b) uses only PutObject. C can be any size.

**Orphans.** Every chunk object belongs to a file the moment it lands, so a crash cannot strand one. To keep it that way:

- Truncate deletes chunks from the highest index down and rewrites the new last chunk. A crash leaves a file truncated part way, which is a valid unflushed state.
- Delete removes a file's side objects (streams, xattrs) first, then its chunks from the highest index down. A crash leaves a shorter file, never a chunk without a file.
- On B2, overwrites and deletes leave hidden versions. The "Keep only the last version" lifecycle rule removes them after a day. **(inferred)** For a backup that overwrites 50 GB a day, that adds roughly 50 to 100 GB of billed storage, about $0.35 to $0.70 a month.
- No multipart upload is ever started, so no abort sweep or lifecycle rule for uploads is needed.

**Memory with no local disk.** Each dirty chunk needs a C-byte buffer until its PUT finishes. With a 256 MiB cap, that is 64 dirty chunks at C = 4 MiB or 4 at C = 64 MiB. When the cap is reached, the server PUTs dirty chunks early. The crash rule allows that: unflushed data may land at any time. The in-memory chunk map for 1 TB at C = 4 MiB is roughly 250,000 entries, tens of MB. **(inferred)**

**5-minute outage.** Chunk PUTs are idempotent, so the server retries with backoff and keeps dirty chunks in memory. FLUSH waits. When the memory cap is reached, WRITE responses are delayed, which is backpressure. A wipeable scratch disk could hold more dirty chunks, but FLUSH still waits for S3. **(inferred)** The SMB server must send interim responses for a long FLUSH so the Mac does not drop the session. Check this against macOS client timeouts. If the server process dies during the outage, unflushed data is lost, which the crash rule allows.

**Verdict.** It fits the crash rule, avoids orphans without GC, needs only basic S3 calls, and stores holes for free.

### 4.3 Layout (c): alternatives considered

- **Per-band delta objects plus periodic merge.** FLUSH writes one object with only the dirty extents. A later merge rebuilds the band with part copy. This gives the fewest bytes per FLUSH. But deltas superseded by a merge must be deleted, and a crash between merge and delete leaves stale deltas. Cleaning them up is a GC in all but name. Reads also need a LIST of deltas. Rejected under #507.
- **Extent objects keyed by offset and length.** Overlapping extents need ordering and compaction, so this is a log-structured store. Rejected for the same reason.
- **Sparse pages inside a chunk** (store only the non-zero 256 KiB pages of a chunk, with a bitmap). This saves storage for partly written chunks. But the bitmap must live in metadata, which LIST does not return. Not worth the complexity. Trimming trailing zeros gets most of the benefit.
- **(b) with a per-file head object** for size and attributes. It costs one extra PUT per FLUSH and brings an ordering problem between head and chunks. Size from LIST is simpler. Attributes and streams can live in small side objects, a separate decision.

Nothing beats (b) on simplicity while meeting #507.

## 5. Recommendation

**Layout (b), fixed 4 MiB chunk objects**, with C as a volume constant. Benchmark 8 MiB as the alternative. 4 MiB keeps write amplification for scattered 256 KiB to 1 MiB writes at 4 to 16 times. 8 MiB halves the object count at twice the amplification. 16 and 64 MiB amplify scattered metadata writes too much.

**Minimal S3 feature set:**

1. PutObject, atomic per key, including overwrite.
2. GetObject with a byte range. HeadObject is optional.
3. DeleteObject. DeleteObjects for batches is optional.
4. ListObjectsV2 with prefix, delimiter and continuation token.
5. Strong read-after-write for GET and LIST after PUT and DELETE.
6. The backend frees overwritten and deleted bytes, either at once (no versioning) or through a bucket lifecycle rule set by the operator.
7. No minimum storage duration on the storage class used.

Not needed: multipart upload, UploadPartCopy, CopyObject, conditional writes, user metadata, versioning, Object Lock, rename, multipart lifecycle rules.

**Do B2 and Garage both provide it?**

| Feature | B2 | Garage |
|---|---|---|
| 1 to 4 | Yes | Yes |
| 5 Strong consistency | Not documented. Get it confirmed or test it | Yes in `consistent` mode, the default |
| 6 Frees old bytes | Yes, with the "Keep only the last version" lifecycle rule (old bytes billed about 1 day) | Yes, no versioning |
| 7 No minimum duration | Yes | Not applicable |

Both provide the set, with the B2 caveats above. AWS S3 Standard with versioning off provides it fully.

**Single-writer guard.** Conditional writes are out of the set because B2 and Garage lack them. Two servers on one bucket can only be guarded best effort, for example a heartbeat object holding a random owner ID that a starting server reads, then waits and reads again. It cannot stop two servers that start at the same moment. This is an owner decision.

**Risks:**

- B2 consistency is undocumented. Lost read-after-write would break read-modify-write and size from LIST.
- B2 needs a manual lifecycle rule. Without it, every overwrite is kept and billed forever.
- Garage durability depends on operator settings: fsync is off by default, and LMDB can corrupt on power loss.
- Object count: about 250,000 per TB at 4 MiB. A full LIST at start is about 250 pages, and the in-memory map is tens of MB.
- Write amplification of up to 16 times for scattered 256 KiB writes, plus a GET for each partly dirtied chunk not in memory.
- Rename of a multi-chunk file is copy then delete. A crash in between leaves both names or a partial new name. Time Machine renames only small plists, which are single chunks, but even then a crash can leave both names.
- A stale PUT that arrives late after a retry is assumed to lose by timestamp. AWS documents "latest timestamp wins". For B2 this is not documented, and for Garage it is inferred from source.
- The SMB FLUSH path must tolerate a 5-minute wait without the Mac dropping the session. Not yet verified.
