# Native Mac acceptance fixture

This helper observes a task-private MinIO service. It does not implement storage,
create Time Machine backups, validate native metadata exports, or claim Mac
acceptance. It reuses the demonstrated **successful chunk PUT response hold**
from `test/e2e/fault_proxy_test.go`. MinIO has already accepted the PUT; the
application has not received the response. This is not an uncommitted-upload or
S3-storage-loss test.

Build from the root module with the existing Go toolchain:

```sh
go build -o "$TASK_ROOT/fixture" ./test/macos/fixture
go test -race ./test/macos/fixture
```

## Process and controls

Start MinIO separately at the immutable Linux fixture source revision, using
only a task-owned data directory and loopback listeners. Then:

```sh
"$TASK_ROOT/fixture" serve \
  --upstream http://127.0.0.1:19000 \
  --listen 127.0.0.1:19001 --control 127.0.0.1:19002 \
  --events "$TASK_ROOT/proxy-events.jsonl"
```

The events path must not already exist; it is created mode 0600. Its parent must
exist. Standard output emits one readiness JSON object with `proxy` and
`control` URLs. Listeners and upstream must use explicit loopback IPs/ports;
no environment HTTP proxies, redirects, public listeners or external endpoints
are used. Point the application's S3 endpoint at the proxy, with path-style
addressing. The proxy preserves incoming Host/signature, path/query, body and
response content. It never logs signed queries, headers or bodies.

Control endpoints (separate listener, not forwarded to MinIO):

- `POST /hold`: arm the next successful chunk PUT response. Returns 409 if
  already armed or a response is pending. Non-chunk/failed PUTs don't consume it.
- `GET /state`: current observations.
- `POST /release`: disarm, release the pending response normally (if any).
  Safe to repeat; it does not remove or change S3 objects.

All successful controls return the same JSON schema:

```json
{
  "observed_at": "RFC3339Nano UTC",
  "armed": false,
  "pending_count": 1,
  "hold": {
    "request_id": 42,
    "path": "/bucket/volume/chunks/0/1",
    "key": "volume/chunks/0/1",
    "held_at": "RFC3339Nano UTC",
    "upstream_status": 200,
    "pending": true,
    "outcome": "upstream_committed_response_pending"
  },
  "chunk_get_success": 15,
  "chunk_get_success_bytes": 123456,
  "evidence_ok": true
}
```

`hold` is null before the first actual hold; thereafter it retains the last
request, even after release/disconnection. `pending_count` counts only held
responses (0 or 1), not all concurrent proxy requests. A concluded hold has
`pending: false`, `ended_at`, and outcome `released` or `client_disconnected`.
A new arm does not erase the last observation; require `pending_count == 1`,
`hold.pending`, and the new request identity/time, not merely non-null `hold`.

The harness must capture active Time Machine status and matching hold state
immediately before killing the application, then its actual exit and the
post-kill proxy state/events. The helper does not make those assertions for it.

The cumulative GET counters count completed successful (including 206 range)
chunk GET response bodies forwarded to the application. They exclude HEAD,
metadata, failed and interrupted responses. Compare before/after counters for
each cold restore, retain the complete events, and also verify full restored
contents/metadata. Counters alone are not Time Machine restore proof.

## Evidence stream

Every JSONL event has `sequence` (monotonic within this process), `time` (UTC),
and `event`. Request events have `request_id`, `method`, `path` and `key` where
nonempty. Optional fields are `upstream_status`, `status` and `bytes` (omitted
when zero). Event names:

- `request_started`, `upstream_response`, `response_complete`,
  `response_interrupted`, `upstream_error`;
- `hold_armed`, `hold_disarmed`, `chunk_put_response_held`,
  `chunk_put_response_released`, `chunk_put_client_disconnected`;
- `metadata_upstream_success`: a 2xx upstream response for a `/meta/` request,
  with method, exact key and observation time. A PUT observation alone **is not
  a validated native recovery point**. Use application success/receipt and
  supported restart/readback validation separately.

Each event is written and synced before publishing corresponding control
state. Write/sync failure latches `evidence_ok: false`, makes controls return
503 and terminates the fixture nonzero. A failing evidence stream cannot pass
silently. Preserve process stderr and exit status too. Application, Time
Machine and MinIO logs remain separate evidence.

## Bucket setup and inventory

Uses the root module's existing AWS Go SDK, not an AWS CLI or new dependency.
Credentials are supplied only through `MINIO_ROOT_USER` and
`MINIO_ROOT_PASSWORD`; region is `us-east-1`, addressing is path-style.

```sh
"$TASK_ROOT/fixture" bucket-create --endpoint http://127.0.0.1:19000 --bucket "$BUCKET"
"$TASK_ROOT/fixture" bucket-list --endpoint http://127.0.0.1:19000 --bucket "$BUCKET"
"$TASK_ROOT/fixture" bucket-list --endpoint http://127.0.0.1:19000 --bucket "$BUCKET" --prefix 'volume/meta/'
```

Create output: `{ "bucket": "...", "created_at": "..." }`.

List output: `{ "bucket": "...", "prefix": "...", "listed_at": "...",
"object_count": N, "total_bytes": N, "objects": [{ "key": "...", "size": N,
"last_modified": "...", "etag": "..." }] }`.

Listings follow all continuation pages and fail on errors or truncation without
progress. Empty proof requires a **prefix-free** listing with `object_count: 0`
and `objects: []`, before application initialization. An inventory while
writes continue is an observation over an interval, not an atomic snapshot.
ETags are opaque S3 metadata, not assumed SHA256 content hashes. Object sizes
are logical object payload bytes, not MinIO's physical disk allocation.

Tests use ordinary local HTTP handlers and SDK fixtures; they prove portable
helper behavior, not live MinIO, Darwin, Apple SMB or Time Machine acceptance.
