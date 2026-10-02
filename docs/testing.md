# Tests

```sh
go vet ./... && go test ./...        # fast tests, no Docker
scripts/test-linux.sh                # every test, with real SMB and MinIO, in Docker
scripts/test-linux.sh -v ./test/e2e  # arguments go to go test
```

`go test ./...` needs Go 1.26.3 and a C compiler. Tests that need MinIO skip
when `S3_SMB_E2E_ENDPOINT` is unset.

`scripts/test-linux.sh` needs Linux, Docker and Bash. It builds `test/Dockerfile`,
which holds Go 1.26.3 and MinIO compiled from commit
`0d7408fc9969caf07de6a8c3a84f9fbb10a6739e`. It starts MinIO on a private Docker
network with test credentials, mounts the source read-only, builds the
application and runs `go test -race` on every package. The script sets
`S3_SMB_E2E_ENDPOINT`, so no test skips. The MinIO data lives in the
container and the script removes it on exit.

The tests in `test/e2e` start the built application, answer its terminal
prompt, and read and write files over signed SMB. Recovery tests delete all
local state and start again from the bucket.

The script prints the directory that holds each daemon's stdout, stderr and
prompt log. Set `S3_SMB_TEST_LOGS` to choose that directory.

Go module and build caches persist in two Docker volumes. Remove them with
`docker volume rm s3-smb-test-gomod s3-smb-test-gobuild`.

GitHub runs both commands on every pull request and on `main`. No Linux test
shows that Time Machine works. See [Hosted-Mac acceptance](macos-acceptance.md).
