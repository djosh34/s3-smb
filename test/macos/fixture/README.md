# Mac test fixture

A small command that creates and lists the MinIO bucket for the Time Machine test
in `test/macos`. It reads the MinIO credentials from `MINIO_ROOT_USER` and
`MINIO_ROOT_PASSWORD` and accepts only a loopback endpoint.

```sh
go build -o fixture ./test/macos/fixture
./fixture bucket-create --endpoint http://127.0.0.1:19000 --bucket time-machine
./fixture bucket-list --endpoint http://127.0.0.1:19000 --bucket time-machine --prefix s3-smb/meta/
```

`bucket-list` prints one JSON object with the key and size of every object
under the prefix.
