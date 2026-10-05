# B2 consistency for the new engine

Research for [#604](https://github.com/djosh34/s3-smb/issues/604), part of the map [#572](https://github.com/djosh34/s3-smb/issues/572).

Question: does Backblaze B2 give the read-after-write and LIST consistency the new engine needs?

Each statement is marked:
- **measured**: seen in our runs against the real bucket.
- **documented**: Backblaze's own docs say so.
- **inferred**: our reading of the measurements or docs, not proven.

## Short answer

Yes, B2 is fit for the engine. In 17,623 checks over 3 runs, no read ever saw stale or missing data. GET, HEAD and ListObjectsV2 always showed a PUT, an overwrite or a DELETE at once. LIST always showed the newest database copy as the highest name (**measured**).

There is one quirk, and the engine must not depend on it. ListObjectVersions marks more than one version of a key as latest when the key's versions span a page boundary (**measured**). The engine only needs ListObjectsV2 and DELETE by version ID, so this does not matter. Do not use IsLatest from ListObjectVersions.

The proof is limited. All runs came from one GitHub runner, against one region (`us-west-001`), over about 20 minutes on one evening (**measured**). Backblaze documents no consistency promise for the S3 API (**documented**, by its absence from the [PutObject page](https://www.backblaze.com/apidocs/s3-put-object)).

## Setup

- Bucket `s3-smb-test` in `us-west-001`, private, with the "Keep only the last version" lifecycle rule. The owner set it up ([#604](https://github.com/djosh34/s3-smb/issues/604) round 17).
- The key is limited to that bucket. It cannot read the Object Lock setting: `GetObjectLockConfiguration` returned 403 AccessDenied (**measured**). The owner says Object Lock is off.
- `GetBucketVersioning` returns `Enabled`, and `GetBucketLifecycleConfiguration` returns 1 rule (**measured**). B2 buckets always keep versions (**documented**, [file versions](https://www.backblaze.com/docs/cloud-storage-file-versions)).
- Program: [`research/b2consistency/main.go`](research/b2consistency/main.go), aws-sdk-go-v2 with checksums only when required and path-style addressing.
- Workflow: [`.github/workflows/b2-consistency.yml`](.github/workflows/b2-consistency.yml). It runs only on `workflow_dispatch`. The secrets reach the program as environment variables and are never printed. The push trigger only registers the workflow, and its job is skipped.
- Writes and reads use two separate S3 clients with their own connection pools. Reads may land on other B2 front ends than the write.
- Every run writes under a fresh prefix `consistency/<time>-<random>/` and deletes every version and delete marker under it at the end, by version ID. All 3 runs left 0 versions (**measured**).
- When a check fails, the program polls every 100 ms for up to 20 s and records how long the anomaly lasted.

## Runs

| Run | Tests | Requests | Checks | Read anomalies | Time |
|---|---|---|---|---|---|
| [37371925322](https://github.com/djosh34/s3-smb/actions/runs/37371925322) | all | 22,910 | 13,021 | 0 | 11m55s |
| [37373418723](https://github.com/djosh34/s3-smb/actions/runs/37373418723) | `newest_copy`, `delete_overwritten` ×1000 with a version check, `if_none_match` | 8,966 | 4,601 | 0 | 6m20s |
| [37374220252](https://github.com/djosh34/s3-smb/actions/runs/37374220252) | `versions_paging` | 48 | 1 | 0 | under 1m |
| **Total** | | **31,924** | **17,623** | **0** | |

Requests count every HTTP attempt, retries included. No request failed in any run, and no retry happened (**measured**).

Run 3 shows as failed in Actions only because the program exits with an error when fewer than 1,000 checks ran. That guard catches broken credentials. The run itself worked and cleaned up.

## Results per test (all measured)

| Test | What it does | Checks | Anomalies |
|---|---|---|---|
| `raw_small` | 1,500 PUTs of 64 B to 4 KiB, then at once GET (body hash), HEAD (size, ETag) and LIST (size, ETag, shown once). The order rotates, so each read is sometimes first. 8 in parallel. | 4,500 | 0 |
| `raw_8mib` | 30 PUTs of 8 MiB, the engine's chunk size, then the same checks. | 90 | 0 |
| `newest_copy` | 600 copies named `%010d-%010d` with increasing numbers, keeping the last 4 and deleting the oldest. After every upload, a LIST must show exactly the expected 4 or 5 names, with the new one highest. Every 25th copy is a 6 MiB multipart upload. Run twice. | 1,200 | 0 |
| `late_copy` | 40 cases where an older copy number finishes after a newer one: 20 as a multipart upload left open while the newer copy lands, 20 as a plain PUT after the newer one. LIST must show both, with the newer name highest. | 40 | 0 |
| `delete` | 1,200 times PUT, DELETE, then GET, HEAD and LIST must not see the key. | 3,600 | 0 |
| `delete_overwritten` | 1,300 times PUT, PUT, DELETE, then GET, HEAD and LIST must not see the key and must not fall back to the older version. In run 2, ListObjectVersions on the key must mark exactly one entry latest, the delete marker. | 3,900 + 1,000 | 0 |
| `overwrite` | 100 keys, each overwritten 10 times. After each PUT, GET, HEAD and LIST must show the newest body, and LIST shows the key once. | 3,000 | 0 |
| `parallel_distinct` | 40 batches of 50 parallel PUTs, then one LIST must show all 50 with the right sizes, plus 5 HEADs. | 240 | 0 |
| `parallel_same_key` | 50 rounds of 8 parallel PUTs to one key. 3 HEADs and a LIST must agree on one of the 8 bodies. | 50 | 0 |
| `hidden_versions` | At the end, ListObjectsV2 over the whole prefix must show exactly the keys whose latest version in ListObjectVersions is not a delete marker. | 3 | 2 runs flagged 1 and 2 keys, see below |

Hidden versions never showed up in ListObjectsV2. Run 1 left 7,410 versions and 2,106 delete markers under the prefix, and ListObjectsV2 showed exactly the 3,764 live keys (**measured**).

## The ListObjectVersions quirk

The final `hidden_versions` check flagged `delow/00131` in run 1 and `delow/00264` and `delow/00931` in run 2. For each, ListObjectVersions over the whole prefix returned two entries with `IsLatest=true`: the delete marker and the newest real version (**measured**). The run 2 detail for `delow/00931`:

```
marker  latest=true  modified=21:09:20.570
version latest=true  modified=21:09:20.470
version latest=false modified=21:09:20.294
```

ListObjectsV2, GET and HEAD were right for those keys. The same keys passed the per-key version check right after the DELETE (**measured**).

Run 3 tested page boundaries directly: 5 keys, each with PUT, PUT, DELETE, which gives 15 entries and 5 latest markers (**measured**):

| Page size | Entries | Entries marked latest | Keys without exactly one latest |
|---|---|---|---|
| default (1000) | 15 | 5 | 0 |
| 1 | 15 | 15 | 5 |
| 2 | 15 | 10 | 5 |
| 4 | 15 | 7 | 2 |

So B2 marks the first entry of a key on each page as latest. The delete markers are always marked right. Only the extra flags are wrong (**measured**). This is a listing bug, not a consistency gap (**inferred**). The engine deletes by version ID and reads state only through ListObjectsV2, so it is not affected (**inferred**).

## For the record

- **Conditional writes** (**measured**): `PutObject` with `If-None-Match: *` returns `501 NotImplemented`, on a new key, an existing key and a deleted key. `If-Match` with the right or a wrong ETag also returns `501`. The object is not written: in all 10 tries the key did not exist after the conditional PUT. B2 rejects these headers instead of ignoring them. This fits [#578](https://github.com/djosh34/s3-smb/issues/578): the design uses no conditional writes. Backblaze's PutObject page does not list these headers (**documented**).
- **Parallel PUTs to one key** (**measured**): all reads agreed on one body in 50 of 50 rounds. The winner was the PUT whose reply reached the client last in only 9 of 50 rounds. The client cannot tell which parallel PUT wins. The engine never uploads one key twice at a time ([#572](https://github.com/djosh34/s3-smb/issues/572) write path), so this is fine.
- **LastModified on late copies**: in 20 of 40 late cases the older copy had the later LastModified, and in 20 it had the same or an earlier one (**measured**). The plain PUT case must give the later time, so the open multipart upload likely got its time from the start of the upload (**inferred**). Either way, the engine must sort copies by name, never by time, as [#589](https://github.com/djosh34/s3-smb/issues/589) already says.
- **DELETE without a version ID only hides** (**documented**, [DeleteObject](https://www.backblaze.com/apidocs/s3-delete-object)): it adds a delete marker and keeps the bytes. Run 1 showed 2,246 keys with hidden versions (**measured**).
- **Lifecycle rule** (**documented**, [lifecycle rules](https://www.backblaze.com/docs/cloud-storage-lifecycle-rules)): "Keep only the last version" sets `daysFromHidingToDeleting: 1`. Rules run once a day, so a hidden version goes 1 to 2 days after it is hidden. Hidden versions stay stored and billed until then (**inferred**: billing is per stored byte, and the docs do not say hidden versions are free).
- **Latency** from a GitHub `ubuntu-24.04-arm` runner (**measured**, run 1): PUT p50 270 ms, p99 870 ms, max 12.6 s. GET p50 56 ms, p99 188 ms, max 7.3 s. HEAD p50 48 ms. LIST p50 96 ms, p99 210 ms. Multipart copy of 6 MiB p50 1.0 s. The worst cases were slow but came with no errors.

## What the docs must say for B2

- Create the bucket private, with **Object Lock off**. With Object Lock, versions cannot be deleted while they are retained, so the engine cannot delete old copies or trashed chunks.
- Set the lifecycle rule **"Keep only the last version of the file"**. A DELETE on B2 only hides the object. Without the rule, every deleted chunk and copy stays stored and billed forever. With it, deleted data is really gone 1 to 2 days after the engine deletes it, on top of the engine's own wait of about 75 minutes.
- Use an application key limited to that one bucket, with read and write.
- The endpoint is `https://s3.<region>.backblazeb2.com`, and the region is the middle part, for example `us-west-001`.
- B2 rejects conditional writes with `501`. The engine does not use them.

## What this does not prove

- No test ran across regions, from other networks, or over days. Consistency held in every check we made, but Backblaze does not promise it.
- The bucket lock's renewals were not tested as such. They use the same PUT, GET and LIST paths tested here.
- No outage or throttling happened during the runs, so B2's 503 behaviour was not seen.
