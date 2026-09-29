# Native S3 transport acceptance

Run from the checkout on the Linux Docker VM:

```sh
scripts/test-linux.sh transport ./internal/storage
```

The same `transport` phase runs in `e2e`, `suite`, and `release`, using the shared
source-pinned MinIO/toolchain image in `test/Dockerfile`. The outer runner holds
`/tmp/s3-smb-heavy.lock`; Go uses `GOMAXPROCS=2`, `-p 2`, a test deadline, and no
module edits. Artifacts include `transport.log` (Go JSON), fixture logs, image
identity, and checkout status. A skipped bare `go test` is not acceptance.

## Boundary and assertions

`internal/storage/transport_integration_test.go:TestTransportAcceptance` runs
`config.Config.Resolve` (including certificate-file loading), `storage.OpenS3`,
and the bundled native S3 implementation. Successful operations are real signed
MinIO Create/Put/Get/Head/List/Delete requests, with an exact binary payload
round-trip, listing/size checks, and not-found after deletion.

A disposable Go TLS reverse proxy forwards these requests to MinIO **without
changing the signed Host, path, or credentials**. It records actual hosts, paths,
TLS versions, verified client-certificate state, and backend status codes. It does
not fake S3 success or bypass MinIO signature validation. The fixture configures
`MINIO_DOMAIN=minio,transport.test`; only the disposable runner's `/etc/hosts`
maps `transport.test` and `transport-test.transport.test` to the proxy. No host
network ports, OS trust-store edits, or committed private keys are used.

Coverage:

- Explicit `path_style: true` and `false`, each with private CA and mutual TLS;
  successful backend responses and exact signed request Host/path assertions
  catch a silent fallback to path-style.
- Wrong CA, default trust without the private CA, and missing client certificate
  fail before any S3 HTTP handler request. A raw-connection observer also rejects
  plaintext downgrade/retry attempts.
- Replacing a CA file leaves an existing startup snapshot unchanged; resolving
  a fresh client sees the replacement and successfully accesses MinIO.
- Explicit HTTP works when deliberately configured; HTTPS failures do not turn
  into HTTP.
- All nine independent access-key/secret-key combinations of inline value, private
  file, and direct executable argv successfully perform real native S3 operations.
- Real MinIO STS credentials are acquired **once by fixture setup**, then supplied
  as an explicit static access/secret/session-token snapshot. Every native request
  must carry that token and succeed, despite poisoned ambient AWS credentials.
  The application does not acquire or refresh it. This tests static-token
  transport, not automatic renewal or unlimited validity of an STS token.
- An invalid signing secret receives an actual MinIO 403. A deliberately stalled
  TLS HTTP response respects a 300ms caller deadline.
- Synthetic credentials/tokens/passwords must not appear in captured native JSON
  diagnostics or checked error strings. This is not a substitute for the broader
  stdout/stderr and helper-output leakage suite.

Certificates are disposable Go-generated ECDSA P-256 CA/server/client certificates
with one-hour validity, written only to private temporary test files. TLS/mTLS
acceptance is at an S3-compatible TLS terminator backed by real MinIO, not a claim
that MinIO itself requires client certificates. These are transport tests, not
SMB-to-S3, full-dataset recovery, AWS compatibility, Linux CI execution, or Mac
Time Machine acceptance evidence.
